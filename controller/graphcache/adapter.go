package graphcache

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	clustercache "github.com/argoproj/argo-cd/gitops-engine/pkg/cache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/kube-openapi/pkg/util/proto"
	"k8s.io/kubectl/pkg/util/openapi"

	statecache "github.com/argoproj/argo-cd/v3/controller/cache"
	"github.com/argoproj/argo-cd/v3/controller/sharding"
	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/db"
	logutils "github.com/argoproj/argo-cd/v3/util/log"
	"github.com/argoproj/argo-cd/v3/util/settings"
)

// GraphLiveStateCache adapts GraphCache to implement the LiveStateCache interface.
// This allows the graph cache to be used as a drop-in replacement for the
// traditional gitops-engine cluster cache.
type GraphLiveStateCache struct {
	config          Config
	clusterCaches   map[string]*clusterCacheAdapter
	lock            sync.RWMutex
	ctx             context.Context
	store           GraphStore
	onObjectUpdated statecache.ObjectUpdatedHandler
	argocdNamespace string
	settingsMgr     *settings.SettingsManager
	db              db.ArgoDB
	clusterSharding sharding.ClusterShardingCache
	repoConn        io.Closer
}

// NewGraphLiveStateCache creates a new adapter that wraps GraphCache
func NewGraphLiveStateCache(config Config, store GraphStore, onObjectUpdated statecache.ObjectUpdatedHandler, argocdNamespace string, settingsMgr *settings.SettingsManager, database db.ArgoDB, clusterSharding sharding.ClusterShardingCache, repoConn io.Closer) *GraphLiveStateCache {
	return &GraphLiveStateCache{
		config:          config,
		clusterCaches:   make(map[string]*clusterCacheAdapter),
		ctx:             context.Background(),
		store:           store,
		onObjectUpdated: onObjectUpdated,
		argocdNamespace: argocdNamespace,
		settingsMgr:     settingsMgr,
		db:              database,
		clusterSharding: clusterSharding,
		repoConn:        repoConn,
	}
}

// GetVersionsInfo returns the Kubernetes server version and API resources
func (a *GraphLiveStateCache) GetVersionsInfo(cluster *appv1.Cluster) (string, []kube.APIResourceInfo, error) {
	clusterCache, err := a.GetClusterCache(cluster)
	if err != nil {
		return "", nil, err
	}

	version := clusterCache.GetServerVersion()
	apiResources := clusterCache.GetAPIResources()
	return version, apiResources, nil
}

// IsNamespaced returns true if the given GroupKind is namespaced
func (a *GraphLiveStateCache) IsNamespaced(cluster *appv1.Cluster, gk schema.GroupKind) (bool, error) {
	clusterCache, err := a.GetClusterCache(cluster)
	if err != nil {
		return false, err
	}

	return clusterCache.IsNamespaced(gk)
}

// GetClusterCache returns the cluster cache for a given server.
func (a *GraphLiveStateCache) GetClusterCache(cluster *appv1.Cluster) (clustercache.ClusterCache, error) {
	if !a.canHandleCluster(cluster) {
		return nil, fmt.Errorf("cluster %s is not managed by this shard", cluster.Server)
	}

	a.lock.RLock()
	clusterCache, ok := a.clusterCaches[cluster.Server]
	a.lock.RUnlock()

	if ok {
		return clusterCache, nil
	}

	a.lock.Lock()
	defer a.lock.Unlock()

	// Double check
	if clusterCache, ok := a.clusterCaches[cluster.Server]; ok {
		return clusterCache, nil
	}

	// Create new GraphCache for this cluster
	restConfig, err := cluster.RESTConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get REST config for cluster %s: %w", cluster.Server, err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client for cluster %s: %w", cluster.Server, err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client for cluster %s: %w", cluster.Server, err)
	}

	// Use shared config as base, override per-cluster fields
	gcConfig := a.config
	gcConfig.DynamicClient = dynamicClient
	gcConfig.DiscoveryClient = discoveryClient
	gcConfig.ServerURL = cluster.Server
	// Use cluster's namespace restrictions (where resources are deployed),
	// NOT ApplicationNamespaces (where Application CRDs live).
	// Empty = watch all namespaces, which is the default for the in-cluster config.
	gcConfig.Namespaces = cluster.Namespaces

	// Use the adapter's context for graph cache lifecycle
	gc, err := NewGraphCache(a.ctx, gcConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create graph cache for cluster %s: %w", cluster.Server, err)
	}

	// Set initial custom label key for pre-filtering
	if a.settingsMgr != nil {
		if labelKey, err := a.settingsMgr.GetAppInstanceLabelKey(); err == nil && labelKey != "" {
			gc.SetAppInstanceLabelKey(labelKey)
		}
	}

	// Wire up custom relationship provider from settings
	if a.settingsMgr != nil {
		settingsMgr := a.settingsMgr
		gc.SetCustomRelationshipProvider(func() ([]CustomRelationshipRule, error) {
			raw, err := settingsMgr.GetResourceRelationshipsRaw()
			if err != nil {
				return nil, err
			}
			return ParseCustomRelationshipRules(raw)
		})
	}

	// Restore from snapshot if available
	if a.store != nil {
		snapshot, loadErr := a.store.LoadSnapshot(cluster.Server)
		if loadErr != nil {
			log.Warnf("Failed to load snapshot for cluster %s: %v", cluster.Server, loadErr)
		} else if snapshot != nil {
			if restoreErr := gc.Restore(snapshot); restoreErr != nil {
				log.Warnf("Failed to restore snapshot for cluster %s: %v", cluster.Server, restoreErr)
			}
		}
	}

	// Start the cache
	if err := gc.Start(); err != nil {
		return nil, fmt.Errorf("failed to start graph cache for cluster %s: %w", cluster.Server, err)
	}

	// Start persistence loop
	if a.store != nil {
		go a.runPersistenceLoop(cluster.Server, gc, gcConfig.GraphConfig.PersistenceInterval)
	}

	clusterCache = newClusterCacheAdapter(gc, cluster.Server, a.onObjectUpdated, a.argocdNamespace)
	// Signal that initial sync is complete since Start() succeeded
	clusterCache.markSynced()
	a.clusterCaches[cluster.Server] = clusterCache
	return clusterCache, nil
}

func (a *GraphLiveStateCache) runPersistenceLoop(clusterServer string, gc *GraphCache, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			snapshot, err := gc.Snapshot()
			if err != nil {
				log.Warnf("Failed to create snapshot for cluster %s: %v", clusterServer, err)
				continue
			}
			if err := a.store.SaveSnapshot(clusterServer, snapshot); err != nil {
				log.Warnf("Failed to save snapshot for cluster %s: %v", clusterServer, err)
			}
		}
	}
}

// IterateHierarchy executes the callback for each resource in the hierarchy starting from the given key
func (a *GraphLiveStateCache) IterateHierarchy(server string, key kube.ResourceKey, action func(child appv1.ResourceNode, appName string) bool) error {
	cluster := &appv1.Cluster{Server: server}
	clusterCache, err := a.GetClusterCache(cluster)
	if err != nil {
		return err
	}

	if adapter, ok := clusterCache.(*clusterCacheAdapter); ok {
		return adapter.IterateHierarchy(key, action)
	}
	return fmt.Errorf("cluster cache does not support IterateHierarchy")
}

// IterateHierarchyV2 iterates resource tree starting from the specified top level resources
func (a *GraphLiveStateCache) IterateHierarchyV2(cluster *appv1.Cluster, keys []kube.ResourceKey, action func(child appv1.ResourceNode, appName string) bool) error {
	clusterCache, err := a.GetClusterCache(cluster)
	if err != nil {
		return err
	}

	if adapter, ok := clusterCache.(*clusterCacheAdapter); ok {
		visited := make(map[kube.ResourceKey]bool)
		for _, key := range keys {
			adapter.iterateHierarchyRecursiveSkipMissing(key, action, visited)
		}
		return nil
	}
	return fmt.Errorf("cluster cache does not support IterateHierarchyV2")
}

// GetManagedLiveObjs returns live objects managed by the application
func (a *GraphLiveStateCache) GetManagedLiveObjs(cluster *appv1.Cluster, app *appv1.Application, targetObjs []*unstructured.Unstructured) (map[kube.ResourceKey]*unstructured.Unstructured, error) {
	clusterCache, err := a.GetClusterCache(cluster)
	if err != nil {
		return nil, err
	}

	if adapter, ok := clusterCache.(*clusterCacheAdapter); ok {
		return adapter.GetManagedLiveObjsForApp(app, targetObjs)
	}
	return nil, fmt.Errorf("cluster cache does not support GetManagedLiveObjsForApp")
}

// IterateResources iterates over all resources in the cache for a given server
func (a *GraphLiveStateCache) IterateResources(cluster *appv1.Cluster, callback func(res *clustercache.Resource, info *statecache.ResourceInfo)) error {
	clusterCache, err := a.GetClusterCache(cluster)
	if err != nil {
		return err
	}

	if adapter, ok := clusterCache.(*clusterCacheAdapter); ok {
		return adapter.IterateResources(callback)
	}
	return fmt.Errorf("cluster cache does not support IterateResources")
}

// GetNamespaceTopLevelResources returns all top-level resources in a namespace
func (a *GraphLiveStateCache) GetNamespaceTopLevelResources(cluster *appv1.Cluster, namespace string) (map[kube.ResourceKey]appv1.ResourceNode, error) {
	clusterCache, err := a.GetClusterCache(cluster)
	if err != nil {
		return nil, err
	}

	if adapter, ok := clusterCache.(*clusterCacheAdapter); ok {
		return adapter.GetNamespaceTopLevelResources(namespace)
	}
	return nil, fmt.Errorf("cluster cache does not support GetNamespaceTopLevelResources")
}

// UpdateShard updates the shard of the cache.
// When the shard changes, cluster caches for clusters no longer managed by this
// shard are invalidated and removed.
func (a *GraphLiveStateCache) UpdateShard(shard int) bool {
	changed := a.clusterSharding.UpdateShard(shard)
	if changed {
		a.lock.Lock()
		for server, cc := range a.clusterCaches {
			if !a.clusterSharding.IsManagedCluster(&appv1.Cluster{Server: server}) {
				cc.Invalidate()
				delete(a.clusterCaches, server)
				log.Infof("Invalidated cluster cache for %s after shard change", server)
			}
		}
		a.lock.Unlock()
	}
	return changed
}

// Run starts the cache and watches for cluster changes.
func (a *GraphLiveStateCache) Run(ctx context.Context) error {
	a.lock.Lock()
	a.ctx = ctx
	a.lock.Unlock()

	if a.settingsMgr != nil {
		go a.watchSettings(ctx)
	}

	kube.RetryUntilSucceed(ctx, clustercache.ClusterRetryTimeout, "watch clusters", logutils.NewLogrusLogger(logutils.NewWithCurrentConfig()), func() error {
		return a.db.WatchClusters(ctx, a.handleAddEvent, a.handleModEvent, a.handleDeleteEvent)
	})

	<-ctx.Done()
	a.shutdownAllClusters()
	return nil
}

func (a *GraphLiveStateCache) canHandleCluster(cluster *appv1.Cluster) bool {
	return a.clusterSharding.IsManagedCluster(cluster)
}

func (a *GraphLiveStateCache) handleAddEvent(cluster *appv1.Cluster) {
	a.clusterSharding.Add(cluster)
	if !a.canHandleCluster(cluster) {
		log.Infof("Ignoring cluster %s (not managed by this shard)", cluster.Server)
		return
	}
	a.lock.RLock()
	_, ok := a.clusterCaches[cluster.Server]
	a.lock.RUnlock()
	if !ok {
		go func() {
			_, _ = a.GetClusterCache(cluster)
		}()
	}
}

func (a *GraphLiveStateCache) handleModEvent(oldCluster *appv1.Cluster, newCluster *appv1.Cluster) {
	a.clusterSharding.Update(oldCluster, newCluster)
	a.lock.RLock()
	existing, ok := a.clusterCaches[newCluster.Server]
	a.lock.RUnlock()
	if ok {
		if !a.canHandleCluster(newCluster) {
			existing.Invalidate()
			a.lock.Lock()
			delete(a.clusterCaches, newCluster.Server)
			a.lock.Unlock()
			return
		}
		forceInvalidate := false
		if newCluster.RefreshRequestedAt != nil {
			info := existing.GetClusterInfo()
			if info.LastCacheSyncTime != nil && info.LastCacheSyncTime.Before(newCluster.RefreshRequestedAt.Time) {
				forceInvalidate = true
			}
		}
		if forceInvalidate {
			existing.Invalidate()
			go func() {
				_ = existing.EnsureSynced()
			}()
		}
	}
}

func (a *GraphLiveStateCache) handleDeleteEvent(clusterServer string) {
	a.clusterSharding.Delete(clusterServer)
	a.lock.Lock()
	existing, ok := a.clusterCaches[clusterServer]
	if ok {
		delete(a.clusterCaches, clusterServer)
	}
	a.lock.Unlock()
	if ok {
		existing.Invalidate()
	}
}

func (a *GraphLiveStateCache) shutdownAllClusters() {
	a.lock.Lock()
	defer a.lock.Unlock()
	for server, cc := range a.clusterCaches {
		cc.Invalidate()
		delete(a.clusterCaches, server)
	}
	if a.repoConn != nil {
		if err := a.repoConn.Close(); err != nil {
			log.Warnf("Failed to close repo server connection: %v", err)
		}
	}
}

// watchSettings watches for settings changes and updates the tracking method
// on all cluster caches when it changes. This mirrors the traditional cache's
// watchSettings behavior in controller/cache/cache.go.
func (a *GraphLiveStateCache) watchSettings(ctx context.Context) {
	updateCh := make(chan *settings.ArgoCDSettings, 1)
	a.settingsMgr.Subscribe(updateCh)

	defer func() {
		a.settingsMgr.Unsubscribe(updateCh)
		close(updateCh)
	}()

	for {
		select {
		case <-updateCh:
			newMethod, err := a.settingsMgr.GetTrackingMethod()
			if err != nil {
				log.Warnf("Failed to get tracking method from settings: %v", err)
				continue
			}

			// Also update custom app instance label key for pre-filtering
			newLabelKey, err := a.settingsMgr.GetAppInstanceLabelKey()
			if err != nil {
				log.Warnf("Failed to get app instance label key from settings: %v", err)
			}

			graphMethod := convertTrackingMethod(newMethod)

			a.lock.Lock()
			// Update the config so new cluster caches get the right tracking method
			a.config.TrackingMethod = graphMethod
			for server, clusterCache := range a.clusterCaches {
				if clusterCache.graphCache != nil {
					if clusterCache.graphCache.getTrackingMethod() != graphMethod {
						log.WithFields(log.Fields{
							"server":    server,
							"oldMethod": clusterCache.graphCache.trackingMethod,
							"newMethod": graphMethod,
						}).Info("Tracking method changed, re-processing resources")
						clusterCache.graphCache.SetTrackingMethod(graphMethod)
					}
					if newLabelKey != "" {
						clusterCache.graphCache.SetAppInstanceLabelKey(newLabelKey)
					}
					clusterCache.graphCache.refreshCustomRelationships()
				}
			}
			a.lock.Unlock()

		case <-ctx.Done():
			log.Info("Shutting down graph cache settings watch")
			return
		}
	}
}

// convertTrackingMethod converts from the settings tracking method string to
// the graph cache's TrackingMethod type.
func convertTrackingMethod(method string) TrackingMethod {
	switch appv1.TrackingMethod(method) {
	case appv1.TrackingMethodAnnotation:
		return TrackingMethodAnnotation
	case appv1.TrackingMethodLabel:
		return TrackingMethodLabel
	case appv1.TrackingMethodAnnotationAndLabel:
		return TrackingMethodAnnotationAndLabel
	default:
		return TrackingMethodAnnotationAndLabel
	}
}

// GetClustersInfo returns information about all clusters
func (a *GraphLiveStateCache) GetClustersInfo() []clustercache.ClusterInfo {
	a.lock.RLock()
	defer a.lock.RUnlock()

	result := make([]clustercache.ClusterInfo, 0, len(a.clusterCaches))
	for _, clusterCache := range a.clusterCaches {
		result = append(result, clusterCache.GetClusterInfo())
	}

	return result
}

// Init initializes the cache
func (a *GraphLiveStateCache) Init() error {
	log.Info("Initializing GraphCache adapter")
	return nil
}

// RegisterCluster registers a cluster with the graph cache
func (a *GraphLiveStateCache) RegisterCluster(server string, clusterCache *clusterCacheAdapter) {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.clusterCaches[server] = clusterCache
}

// nodeToResourceNode converts a ResourceNode from graph cache to appv1.ResourceNode
func nodeToResourceNode(node *ResourceNode) appv1.ResourceNode {
	parentRefs := make([]appv1.ResourceRef, 0, len(node.Parents))
	for _, parent := range node.Parents {
		parentRefs = append(parentRefs, appv1.ResourceRef{
			Group:     parent.ResourceKey.Group,
			Kind:      parent.ResourceKey.Kind,
			Namespace: parent.ResourceKey.Namespace,
			Name:      parent.ResourceKey.Name,
			UID:       parent.UID,
		})
	}

	creationTimestamp := metav1.NewTime(node.CreatedAt)

	rn := appv1.ResourceNode{
		ResourceRef: appv1.ResourceRef{
			UID:       string(node.UID),
			Name:      node.Key.Name,
			Group:     node.Key.Group,
			Version:   node.Version,
			Kind:      node.Key.Kind,
			Namespace: node.Key.Namespace,
		},
		ParentRefs:      parentRefs,
		ResourceVersion: node.ResourceVersion,
		CreatedAt:       &creationTimestamp,
	}

	// Populate enrichment info from CachedInfo if available
	if info, ok := node.CachedInfo.(*statecache.ResourceInfo); ok && info != nil {
		rn.Info = info.Info
		rn.Images = info.Images
		if info.Health != nil {
			rn.Health = &appv1.HealthStatus{
				Status:  info.Health.Status,
				Message: info.Health.Message,
			}
		}
		rn.NetworkingInfo = info.NetworkingInfo
	}

	return rn
}

// nodeToCacheResource converts a ResourceNode to a gitops-engine cache.Resource
func nodeToCacheResource(node *ResourceNode) *clustercache.Resource {
	res := &clustercache.Resource{
		Ref: v1.ObjectReference{
			APIVersion: schema.GroupVersion{Group: node.Key.Group, Version: node.Version}.String(),
			Kind:       node.Key.Kind,
			Namespace:  node.Key.Namespace,
			Name:       node.Key.Name,
			UID:        types.UID(node.UID),
		},
		ResourceVersion: node.ResourceVersion,
		Info:            node.CachedInfo,
		Resource:        node.Resource,
	}
	if node.Info != nil {
		res.OwnerRefs = node.Info.OwnerRefs
	}
	return res
}

// clusterCacheAdapter wraps a single cluster's cache operations and implements
// the cache.ClusterCache interface from gitops-engine.
type clusterCacheAdapter struct {
	graphCache      *GraphCache
	server          string
	argocdNamespace string // ArgoCD installation namespace for computing app instance names

	// Handler management
	handlersLock            sync.Mutex
	handlerKey              uint64
	resourceUpdatedHandlers map[uint64]clustercache.OnResourceUpdatedHandler
	eventHandlers           map[uint64]clustercache.OnEventHandler
	processEventsHandlers   map[uint64]clustercache.OnProcessEventsHandler

	// Sync state — syncedCh is closed when initial discovery completes
	synced    atomic.Bool
	syncMu    sync.Mutex // Protects syncedCh and syncOnce
	syncedCh  chan struct{}
	syncOnce  sync.Once

	// Cached GVK parser (lazy-initialized, cleared on Invalidate)
	gvkParser     *managedfields.GvkParser
	gvkParserLock sync.Mutex
}

func newClusterCacheAdapter(gc *GraphCache, server string, onObjectUpdated statecache.ObjectUpdatedHandler, argocdNamespace string) *clusterCacheAdapter {
	adapter := &clusterCacheAdapter{
		graphCache:              gc,
		server:                  server,
		argocdNamespace:         argocdNamespace,
		resourceUpdatedHandlers: make(map[uint64]clustercache.OnResourceUpdatedHandler),
		eventHandlers:           make(map[uint64]clustercache.OnEventHandler),
		processEventsHandlers:   make(map[uint64]clustercache.OnProcessEventsHandler),
		syncedCh:                make(chan struct{}),
	}
	// synced starts as false — will be signaled after Start() completes

	// Wire up resource update notifications from GraphCache to adapter handlers
	gc.SetResourceUpdateCallback(func(newRes, oldRes *ResourceNode, obj *unstructured.Unstructured, eventType watch.EventType) {
		// Notify OnEvent handlers
		adapter.notifyEvent(eventType, obj)

		// Build cache.Resource for OnResourceUpdated handlers
		var newCacheRes, oldCacheRes *clustercache.Resource
		if newRes != nil {
			newCacheRes = nodeToCacheResource(newRes)
		}
		if oldRes != nil {
			oldCacheRes = nodeToCacheResource(oldRes)
		}

		// Build namespace resources map
		ns := ""
		if newRes != nil {
			ns = newRes.Key.Namespace
		} else if oldRes != nil {
			ns = oldRes.Key.Namespace
		}

		var nsResources map[kube.ResourceKey]*clustercache.Resource
		if ns != "" && gc.graph != nil {
			nsResources = make(map[kube.ResourceKey]*clustercache.Resource)
			allNodes := gc.graph.GetAllNodes()
			for _, node := range allNodes {
				if node.Key.Namespace == ns {
					nsResources[node.Key] = nodeToCacheResource(node)
				}
			}
		}

		adapter.notifyResourceUpdated(newCacheRes, oldCacheRes, nsResources)

		// Notify the controller's ObjectUpdatedHandler so it re-queues affected apps.
		// This is critical: without this, the controller won't know to re-reconcile
		// when resources change via watch events.
		if onObjectUpdated != nil {
			toNotify := make(map[string]bool)
			var ref v1.ObjectReference
			if newRes != nil {
				ref = v1.ObjectReference{
					APIVersion: schema.GroupVersion{Group: newRes.Key.Group, Version: newRes.Version}.String(),
					Kind:       newRes.Key.Kind,
					Namespace:  newRes.Key.Namespace,
					Name:       newRes.Key.Name,
					UID:        types.UID(newRes.UID),
				}
			} else if oldRes != nil {
				ref = v1.ObjectReference{
					APIVersion: schema.GroupVersion{Group: oldRes.Key.Group, Version: oldRes.Version}.String(),
					Kind:       oldRes.Key.Kind,
					Namespace:  oldRes.Key.Namespace,
					Name:       oldRes.Key.Name,
					UID:        types.UID(oldRes.UID),
				}
			}

			for _, r := range []*ResourceNode{newRes, oldRes} {
				if r == nil {
					continue
				}
				if r.ManagedBy != "" {
					isRoot := len(r.Parents) == 0
					toNotify[r.ManagedBy] = isRoot || toNotify[r.ManagedBy]
				}
			}

			if len(toNotify) > 0 {
				onObjectUpdated(toNotify, ref)
			}
		}
	})

	return adapter
}

// GetServerVersion returns the Kubernetes server version
func (c *clusterCacheAdapter) GetServerVersion() string {
	if c.graphCache == nil || c.graphCache.watchManager == nil {
		log.Warn("GetServerVersion called but graphCache or watchManager is nil")
		return ""
	}
	info, err := c.graphCache.watchManager.GetDiscoveryClient().ServerVersion()
	if err != nil {
		log.Warnf("Failed to get server version: %v", err)
		return ""
	}
	return info.GitVersion
}

// GetAPIResources returns the list of API resources
func (c *clusterCacheAdapter) GetAPIResources() []kube.APIResourceInfo {
	if c.graphCache == nil || c.graphCache.watchManager == nil {
		log.Warn("GetAPIResources called but graphCache or watchManager is nil")
		return []kube.APIResourceInfo{}
	}
	lists, err := c.graphCache.watchManager.GetDiscoveryClient().ServerPreferredResources()
	if err != nil {
		log.Warnf("Failed to get API resources: %v", err)
		return []kube.APIResourceInfo{}
	}

	var result []kube.APIResourceInfo
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			result = append(result, kube.APIResourceInfo{
				GroupKind: schema.GroupKind{Group: gv.Group, Kind: r.Kind},
				GroupVersionResource: schema.GroupVersionResource{
					Group:    gv.Group,
					Version:  gv.Version,
					Resource: r.Name,
				},
				Meta: r,
			})
		}
	}
	return result
}

// IsNamespaced returns whether a GroupKind is namespaced
func (c *clusterCacheAdapter) IsNamespaced(gk schema.GroupKind) (bool, error) {
	if c.graphCache == nil || c.graphCache.watchManager == nil {
		return false, fmt.Errorf("graphCache or watchManager is nil")
	}
	// First check if we already watch this type
	c.graphCache.watchManager.watchLock.RLock()
	handle, exists := c.graphCache.watchManager.watches[gk]
	c.graphCache.watchManager.watchLock.RUnlock()

	if exists {
		return handle.IsNamespaced, nil
	}

	// Fallback to discovery
	resources := c.GetAPIResources()
	for _, r := range resources {
		if r.GroupKind == gk {
			return r.Meta.Namespaced, nil
		}
	}

	return false, fmt.Errorf("resource not found: %s", gk)
}

// GetClusterInfo returns cluster information
func (c *clusterCacheAdapter) GetClusterInfo() clustercache.ClusterInfo {
	if c.graphCache == nil {
		return clustercache.ClusterInfo{Server: c.server}
	}
	metrics := c.graphCache.GetMetrics()

	info := clustercache.ClusterInfo{
		Server:         c.server,
		ResourcesCount: metrics.TotalManagedResources,
		APIsCount:      metrics.ActiveWatches,
		K8SVersion:     c.GetServerVersion(),
	}

	if !metrics.LastDiscoveryTime.IsZero() {
		t := metrics.LastDiscoveryTime
		info.LastCacheSyncTime = &t
	}

	return info
}

// IterateHierarchy executes the callback for each resource in the hierarchy starting from the given key
func (c *clusterCacheAdapter) IterateHierarchy(key kube.ResourceKey, action func(child appv1.ResourceNode, appName string) bool) error {
	visited := make(map[kube.ResourceKey]bool)
	return c.iterateHierarchyRecursive(key, action, visited)
}

func (c *clusterCacheAdapter) iterateHierarchyRecursive(key kube.ResourceKey, action func(child appv1.ResourceNode, appName string) bool, visited map[kube.ResourceKey]bool) error {
	if visited[key] {
		return nil
	}
	visited[key] = true

	node, exists := c.graphCache.graph.Get(key)
	if !exists {
		return fmt.Errorf("resource not found: %v", key)
	}

	resNode := nodeToResourceNode(node)
	if !action(resNode, node.ManagedBy) {
		return nil
	}

	children := c.graphCache.graph.GetChildren(key)
	for _, child := range children {
		if err := c.iterateHierarchyRecursive(child.Key, action, visited); err != nil {
			return err
		}
	}

	return nil
}

// iterateHierarchyRecursiveSkipMissing silently skips missing resources instead of
// returning errors. Used by IterateHierarchyV2 which may receive keys for resources
// not yet in cache.
func (c *clusterCacheAdapter) iterateHierarchyRecursiveSkipMissing(key kube.ResourceKey, action func(child appv1.ResourceNode, appName string) bool, visited map[kube.ResourceKey]bool) {
	if visited[key] {
		return
	}
	visited[key] = true

	node, exists := c.graphCache.graph.Get(key)
	if !exists {
		return
	}

	resNode := nodeToResourceNode(node)
	if !action(resNode, node.ManagedBy) {
		return
	}

	children := c.graphCache.graph.GetChildren(key)
	for _, child := range children {
		c.iterateHierarchyRecursiveSkipMissing(child.Key, action, visited)
	}
}

// IterateHierarchyV2 iterates resource tree starting from the specified top level resources
// and provides namespace resources to the callback for context.
// Handles both within-namespace and cross-namespace parent-child relationships.
func (c *clusterCacheAdapter) IterateHierarchyV2(keys []kube.ResourceKey, action func(resource *clustercache.Resource, namespaceResources map[kube.ResourceKey]*clustercache.Resource) bool) {
	// Build namespace resource maps lazily
	nsResourceCache := make(map[string]map[kube.ResourceKey]*clustercache.Resource)
	getNsResources := func(namespace string) map[kube.ResourceKey]*clustercache.Resource {
		if namespace == "" {
			return nil
		}
		if cached, ok := nsResourceCache[namespace]; ok {
			return cached
		}
		nsMap := make(map[kube.ResourceKey]*clustercache.Resource)
		allNodes := c.graphCache.graph.GetAllNodes()
		for _, node := range allNodes {
			if node.Key.Namespace == namespace {
				nsMap[node.Key] = nodeToCacheResource(node)
			}
		}
		nsResourceCache[namespace] = nsMap
		return nsMap
	}

	visited := make(map[kube.ResourceKey]bool)

	var traverse func(key kube.ResourceKey)
	traverse = func(key kube.ResourceKey) {
		if visited[key] {
			return
		}
		visited[key] = true

		node, exists := c.graphCache.graph.Get(key)
		if !exists {
			return
		}

		res := nodeToCacheResource(node)
		nsResources := getNsResources(node.Key.Namespace)

		if !action(res, nsResources) {
			return
		}

		// Traverse direct children (may be cross-namespace)
		children := c.graphCache.graph.GetChildren(key)
		for _, child := range children {
			traverse(child.Key)
		}

		// For cluster-scoped resources, also check for cross-namespace children
		// that reference this resource by UID via OwnerReferences.
		// This handles Crossplane-style hierarchies where cluster-scoped
		// resources own namespaced resources in different namespaces.
		if node.Key.Namespace == "" && node.UID != "" {
			c.traverseCrossNamespaceChildren(node.UID, visited, traverse)
		}
	}

	for _, key := range keys {
		traverse(key)
	}
}

// traverseCrossNamespaceChildren finds children that reference the given parent UID
// across all namespaces. Used for cluster-scoped parents owning namespaced children.
func (c *clusterCacheAdapter) traverseCrossNamespaceChildren(parentUID string, visited map[kube.ResourceKey]bool, traverse func(kube.ResourceKey)) {
	allNodes := c.graphCache.graph.GetAllNodes()
	for _, node := range allNodes {
		if visited[node.Key] {
			continue
		}
		for _, parent := range node.Parents {
			if parent.UID == parentUID {
				traverse(node.Key)
				break
			}
		}
	}
}

// GetManagedLiveObjsForApp returns live objects managed by the application.
// Uses a two-phase approach matching the traditional gitops-engine cache:
// Phase 1: Collect root resources from cache that belong to this app.
// Phase 2: For each targetObj not found, fetch from API and auto-discover the type.
func (c *clusterCacheAdapter) GetManagedLiveObjsForApp(app *appv1.Application, targetObjs []*unstructured.Unstructured) (map[kube.ResourceKey]*unstructured.Unstructured, error) {
	appName := app.InstanceName(c.argocdNamespace)
	result := make(map[kube.ResourceKey]*unstructured.Unstructured)

	// Phase 1: Collect root managed resources from cache
	resources := c.graphCache.GetResourcesByApplication(appName)
	for _, res := range resources {
		if res.Resource != nil && len(res.Parents) == 0 {
			result[res.Key] = res.Resource.DeepCopy()
		}
	}

	// Phase 2: For each targetObj not in result, try cache lookup then API fallback
	for _, targetObj := range targetObjs {
		key := kube.GetResourceKey(targetObj)
		if _, exists := result[key]; exists {
			continue
		}

		obj, err := c.resolveResourceForSync(key)
		if err != nil {
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"key":       key,
			}).WithError(err).Warn("Failed to resolve resource for sync")
			continue
		}
		if obj != nil {
			result[key] = obj
		}
	}

	return result, nil
}

// resolveResourceForSync resolves a resource for sync operations.
// It tries cache first, then falls back to the Kubernetes API.
// If the resource type is not watched, it starts a watch and discovers descendants.
//
// IMPORTANT: We always fall back to the API when a resource is not in cache,
// even if the type is watched. A watch may have just been started (e.g., when
// processing an earlier targetObj in the same GetManagedLiveObjs call) and its
// initial LIST may not have completed yet. Only a NotFound from the API means
// the resource truly doesn't exist.
func (c *clusterCacheAdapter) resolveResourceForSync(key kube.ResourceKey) (*unstructured.Unstructured, error) {
	if c.graphCache == nil || c.graphCache.graph == nil {
		return nil, fmt.Errorf("graphCache or graph is nil")
	}
	// Check if resource exists in cache
	if node, exists := c.graphCache.graph.Get(key); exists {
		if node.Resource != nil {
			return node.Resource.DeepCopy(), nil
		}
		// In cache but no resource body — fetch from API
		return c.fetchResourceFromAPI(key)
	}

	// Not in cache — always fetch from API.
	// We cannot assume "watched type + not in cache = doesn't exist" because
	// the watch might have been started moments ago and not yet synced.
	obj, err := c.fetchResourceFromAPI(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // Resource truly doesn't exist
		}
		return nil, err
	}
	if obj != nil {
		// Add to graph cache
		c.graphCache.addResourceToGraph(obj)
		// Start watching this type and its descendants if not already watched
		gk := schema.GroupKind{Group: key.Group, Kind: key.Kind}
		if !c.graphCache.watchManager.IsTypeWatched(gk) {
			c.graphCache.ensureWatchAndDescendants(gk)
		}
	}
	return obj, nil
}

// isNamespaceManaged returns whether a namespace is in the managed set.
// Returns true if no namespace restrictions are configured (all namespaces managed).
func (c *clusterCacheAdapter) isNamespaceManaged(ns string) bool {
	if len(c.graphCache.namespaces) == 0 {
		return true
	}
	for _, managed := range c.graphCache.namespaces {
		if managed == ns {
			return true
		}
	}
	return false
}

// fetchResourceFromAPI fetches a single resource from the Kubernetes API.
func (c *clusterCacheAdapter) fetchResourceFromAPI(key kube.ResourceKey) (*unstructured.Unstructured, error) {
	if c.graphCache == nil || c.graphCache.watchManager == nil {
		return nil, fmt.Errorf("graphCache or watchManager is nil")
	}

	// Discover the GVR for this resource
	gk := schema.GroupKind{Group: key.Group, Kind: key.Kind}
	gvr, _, err := c.graphCache.watchManager.discoverResource(gk)
	if err != nil {
		return nil, fmt.Errorf("failed to discover resource %s: %w", gk, err)
	}

	dynClient := c.graphCache.GetDynamicClient()
	if dynClient == nil {
		return nil, fmt.Errorf("dynamic client is nil for cluster %s", c.server)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var obj *unstructured.Unstructured
	if key.Namespace != "" {
		obj, err = dynClient.Resource(gvr).Namespace(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	} else {
		obj, err = dynClient.Resource(gvr).Get(ctx, key.Name, metav1.GetOptions{})
	}
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// GetManagedLiveObjs returns managed objects matching the isManaged predicate.
// Uses a two-phase approach matching the traditional gitops-engine cache:
// Phase 1: Collect root managed resources from cache using the isManaged predicate.
// Phase 2: For each targetObj not found, fetch from API and auto-discover the type.
func (c *clusterCacheAdapter) GetManagedLiveObjs(targetObjs []*unstructured.Unstructured, isManaged func(r *clustercache.Resource) bool) (map[kube.ResourceKey]*unstructured.Unstructured, error) {
	// Validate namespace management
	for _, obj := range targetObjs {
		ns := obj.GetNamespace()
		if len(c.graphCache.namespaces) > 0 && ns != "" && !c.isNamespaceManaged(ns) {
			gvk := obj.GroupVersionKind()
			return nil, fmt.Errorf("namespace %q for %s %q is not managed", ns, gvk.Kind, obj.GetName())
		}
	}

	result := make(map[kube.ResourceKey]*unstructured.Unstructured)

	// Phase 1: Collect root managed resources from cache
	allNodes := c.graphCache.graph.GetAllNodes()
	for _, node := range allNodes {
		if node.Resource == nil || len(node.Parents) > 0 {
			continue
		}
		res := nodeToCacheResource(node)
		if isManaged != nil && !isManaged(res) {
			continue
		}
		result[node.Key] = node.Resource.DeepCopy()
	}

	// Phase 2: For each targetObj not in result, try cache lookup then API fallback
	for _, targetObj := range targetObjs {
		key := kube.GetResourceKey(targetObj)
		if _, exists := result[key]; exists {
			continue
		}

		obj, err := c.resolveResourceForSync(key)
		if err != nil {
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"key":       key,
			}).WithError(err).Warn("Failed to resolve resource for sync")
			continue
		}
		if obj != nil {
			result[key] = obj
		}
	}

	return result, nil
}

// IterateResources iterates over all resources in the cache
func (c *clusterCacheAdapter) IterateResources(callback func(res *clustercache.Resource, info *statecache.ResourceInfo)) error {
	allResources := c.graphCache.graph.GetAllNodes()

	for _, node := range allResources {
		resource := nodeToCacheResource(node)
		var info *statecache.ResourceInfo
		if ri, ok := node.CachedInfo.(*statecache.ResourceInfo); ok && ri != nil {
			info = ri
		} else {
			info = &statecache.ResourceInfo{
				AppName: node.ManagedBy,
			}
		}
		callback(resource, info)
	}
	return nil
}

// GetNamespaceTopLevelResources returns all top-level resources in a namespace
func (c *clusterCacheAdapter) GetNamespaceTopLevelResources(namespace string) (map[kube.ResourceKey]appv1.ResourceNode, error) {
	result := make(map[kube.ResourceKey]appv1.ResourceNode)
	allResources := c.graphCache.graph.GetAllNodes()

	for _, node := range allResources {
		if node.Key.Namespace != namespace {
			continue
		}

		if len(node.Parents) == 0 {
			result[node.Key] = nodeToResourceNode(node)
		}
	}

	return result, nil
}

// EnsureSynced blocks until the cache is synced (initial discovery complete).
// Returns error if sync doesn't complete within 30 seconds.
func (c *clusterCacheAdapter) EnsureSynced() error {
	if c.synced.Load() {
		return nil
	}

	c.syncMu.Lock()
	ch := c.syncedCh
	c.syncMu.Unlock()

	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-ch:
		return nil
	case <-timer.C:
		return fmt.Errorf("timeout waiting for cache sync for cluster %s", c.server)
	}
}

// markSynced signals that initial sync is complete. Safe to call multiple times.
func (c *clusterCacheAdapter) markSynced() {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	c.syncOnce.Do(func() {
		c.synced.Store(true)
		close(c.syncedCh)
	})
}

// GetOpenAPISchema returns the OpenAPI schema
func (c *clusterCacheAdapter) GetOpenAPISchema() openapi.Resources {
	if c.graphCache == nil || c.graphCache.watchManager == nil {
		log.Warn("GetOpenAPISchema called but graphCache or watchManager is nil")
		return nil
	}
	doc, err := c.graphCache.watchManager.GetDiscoveryClient().OpenAPISchema()
	if err != nil {
		log.Warnf("Failed to get OpenAPI schema: %v", err)
		return nil
	}

	resources, err := openapi.NewOpenAPIData(doc)
	if err != nil {
		log.Warnf("Failed to parse OpenAPI schema: %v", err)
		return nil
	}
	return resources
}

// GetGVKParser returns the GVK parser, creating it lazily from the OpenAPI schema.
func (c *clusterCacheAdapter) GetGVKParser() *managedfields.GvkParser {
	c.gvkParserLock.Lock()
	defer c.gvkParserLock.Unlock()

	if c.gvkParser != nil {
		return c.gvkParser
	}

	if c.graphCache == nil || c.graphCache.watchManager == nil {
		log.Warn("GetGVKParser called but graphCache or watchManager is nil")
		return nil
	}

	doc, err := c.graphCache.watchManager.GetDiscoveryClient().OpenAPISchema()
	if err != nil {
		log.Warnf("Failed to get OpenAPI schema for GVKParser: %v", err)
		return nil
	}

	models, err := proto.NewOpenAPIData(doc)
	if err != nil {
		log.Warnf("Failed to parse OpenAPI data for GVKParser: %v", err)
		return nil
	}

	parser, err := managedfields.NewGVKParser(models, false)
	if err != nil {
		log.Warnf("Failed to create GVKParser: %v", err)
		return nil
	}

	c.gvkParser = parser
	return c.gvkParser
}

// Invalidate invalidates the cache, triggering a re-discovery.
// Resets sync state, stops all watches, and re-runs discovery.
// Note: UpdateSettingsFunc options are designed for the gitops-engine clusterCache
// and cannot be directly applied. Settings changes should be handled at the
// GraphLiveStateCache level by re-creating the adapter.
func (c *clusterCacheAdapter) Invalidate(opts ...clustercache.UpdateSettingsFunc) {
	if len(opts) > 0 {
		log.WithField("component", "graph-cache").
			Warn("Invalidate called with UpdateSettingsFunc options which are not supported by graph cache adapter")
	}

	// Reset sync state under lock to prevent races with EnsureSynced/markSynced
	c.syncMu.Lock()
	c.synced.Store(false)
	c.syncOnce = sync.Once{}
	c.syncedCh = make(chan struct{})
	c.syncMu.Unlock()

	// Stop all watches
	if c.graphCache != nil && c.graphCache.watchManager != nil {
		c.graphCache.watchManager.StopAllWatches()
	}

	// Clear GVK parser cache
	c.gvkParserLock.Lock()
	c.gvkParser = nil
	c.gvkParserLock.Unlock()

	// Re-run discovery and signal sync
	go func() {
		if err := c.graphCache.DiscoverManagedResources(); err != nil {
			log.WithError(err).Warn("Failed to re-discover resources after invalidation")
		}
		c.markSynced()
	}()
}

// FindResources finds resources matching the given predicates
func (c *clusterCacheAdapter) FindResources(namespace string, predicates ...func(r *clustercache.Resource) bool) map[kube.ResourceKey]*clustercache.Resource {
	result := make(map[kube.ResourceKey]*clustercache.Resource)
	allNodes := c.graphCache.graph.GetAllNodes()

	for _, node := range allNodes {
		if namespace != "" && node.Key.Namespace != namespace {
			continue
		}

		res := nodeToCacheResource(node)

		match := true
		for _, p := range predicates {
			if !p(res) {
				match = false
				break
			}
		}

		if match {
			result[node.Key] = res
		}
	}
	return result
}

// OnResourceUpdated registers a handler that is called when a resource is updated in the cache.
// Returns an unsubscribe function to remove the handler.
func (c *clusterCacheAdapter) OnResourceUpdated(handler clustercache.OnResourceUpdatedHandler) clustercache.Unsubscribe {
	c.handlersLock.Lock()
	defer c.handlersLock.Unlock()

	c.handlerKey++
	key := c.handlerKey
	c.resourceUpdatedHandlers[key] = handler

	return func() {
		c.handlersLock.Lock()
		defer c.handlersLock.Unlock()
		delete(c.resourceUpdatedHandlers, key)
	}
}

// OnEvent registers a handler that is called for every Kubernetes watch event.
// Returns an unsubscribe function to remove the handler.
func (c *clusterCacheAdapter) OnEvent(handler clustercache.OnEventHandler) clustercache.Unsubscribe {
	c.handlersLock.Lock()
	defer c.handlersLock.Unlock()

	c.handlerKey++
	key := c.handlerKey
	c.eventHandlers[key] = handler

	return func() {
		c.handlersLock.Lock()
		defer c.handlersLock.Unlock()
		delete(c.eventHandlers, key)
	}
}

// OnProcessEventsHandler registers a handler that is called when events are processed.
// Returns an unsubscribe function to remove the handler.
func (c *clusterCacheAdapter) OnProcessEventsHandler(handler clustercache.OnProcessEventsHandler) clustercache.Unsubscribe {
	c.handlersLock.Lock()
	defer c.handlersLock.Unlock()

	c.handlerKey++
	key := c.handlerKey
	c.processEventsHandlers[key] = handler

	return func() {
		c.handlersLock.Lock()
		defer c.handlersLock.Unlock()
		delete(c.processEventsHandlers, key)
	}
}

// notifyResourceUpdated calls all registered OnResourceUpdated handlers.
// Called by the graph cache when a resource changes.
func (c *clusterCacheAdapter) notifyResourceUpdated(newRes *clustercache.Resource, oldRes *clustercache.Resource, nsResources map[kube.ResourceKey]*clustercache.Resource) {
	c.handlersLock.Lock()
	handlers := make([]clustercache.OnResourceUpdatedHandler, 0, len(c.resourceUpdatedHandlers))
	for _, h := range c.resourceUpdatedHandlers {
		handlers = append(handlers, h)
	}
	c.handlersLock.Unlock()

	for _, h := range handlers {
		h(newRes, oldRes, nsResources)
	}
}

// notifyEvent calls all registered OnEvent handlers.
func (c *clusterCacheAdapter) notifyEvent(eventType watch.EventType, un *unstructured.Unstructured) {
	c.handlersLock.Lock()
	handlers := make([]clustercache.OnEventHandler, 0, len(c.eventHandlers))
	for _, h := range c.eventHandlers {
		handlers = append(handlers, h)
	}
	c.handlersLock.Unlock()

	for _, h := range handlers {
		h(eventType, un)
	}
}
