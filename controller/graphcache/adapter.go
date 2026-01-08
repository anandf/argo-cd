package graphcache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/cache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/kubectl/pkg/util/openapi"

	statecache "github.com/argoproj/argo-cd/v3/controller/cache"
	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// GraphLiveStateCache adapts GraphCache to implement the LiveStateCache interface
// This allows the graph cache to be used as a drop-in replacement for the
// traditional gitops-engine cluster cache
type GraphLiveStateCache struct {
	config        Config
	clusterCaches map[string]*clusterCacheAdapter
	lock          sync.RWMutex
	ctx           context.Context
	store         GraphStore
}

// NewGraphLiveStateCache creates a new adapter that wraps GraphCache
func NewGraphLiveStateCache(config Config, store GraphStore) *GraphLiveStateCache {
	return &GraphLiveStateCache{
		config:        config,
		clusterCaches: make(map[string]*clusterCacheAdapter),
		ctx:           context.Background(),
		store:         store,
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

// GetClusterCache returns the cluster cache for a given server
func (a *GraphLiveStateCache) GetClusterCache(cluster *appv1.Cluster) (cache.ClusterCache, error) {
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

	// Use shared config as base, override clients
	gcConfig := a.config
	gcConfig.DynamicClient = dynamicClient
	gcConfig.DiscoveryClient = discoveryClient
	gcConfig.ServerURL = cluster.Server // Set server URL for metrics

	// Use the adapter's context for graph cache lifecycle
	gc, err := NewGraphCache(a.ctx, gcConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create graph cache for cluster %s: %w", cluster.Server, err)
	}

	// Restore from snapshot if available
	if a.store != nil {
		snapshot, err := a.store.LoadSnapshot(cluster.Server)
		if err != nil {
			log.Warnf("Failed to load snapshot for cluster %s: %v", cluster.Server, err)
		} else if snapshot != nil {
			if err := gc.Restore(snapshot); err != nil {
				log.Warnf("Failed to restore snapshot for cluster %s: %v", cluster.Server, err)
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

	clusterCache = &clusterCacheAdapter{
		graphCache: gc,
		server:     cluster.Server,
	}

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
		for _, key := range keys {
			if err := adapter.IterateHierarchy(key, action); err != nil {
				return err
			}
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
func (a *GraphLiveStateCache) IterateResources(cluster *appv1.Cluster, callback func(res *cache.Resource, info *statecache.ResourceInfo)) error {
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

// UpdateShard updates the shard of the cache
func (a *GraphLiveStateCache) UpdateShard(shard int) bool {
	// GraphCache currently handles all resources or sharding is managed externally/not applicable
	return true
}

// Run starts the cache
func (a *GraphLiveStateCache) Run(ctx context.Context) error {
	a.ctx = ctx
	return nil
}

// GetClustersInfo returns information about all clusters
func (a *GraphLiveStateCache) GetClustersInfo() []cache.ClusterInfo {
	a.lock.RLock()
	defer a.lock.RUnlock()

	result := make([]cache.ClusterInfo, 0, len(a.clusterCaches))
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

	return appv1.ResourceNode{
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
}

// nodeToCacheResource converts a ResourceNode to a gitops-engine cache.Resource
func nodeToCacheResource(node *ResourceNode) *cache.Resource {
	res := &cache.Resource{
		Ref: v1.ObjectReference{
			APIVersion: schema.GroupVersion{Group: node.Key.Group, Version: node.Version}.String(),
			Kind:       node.Key.Kind,
			Namespace:  node.Key.Namespace,
			Name:       node.Key.Name,
			UID:        types.UID(node.UID),
		},
		ResourceVersion: node.ResourceVersion,
	}
	if node.Info != nil {
		res.OwnerRefs = node.Info.OwnerRefs
	}
	return res
}

// clusterCacheAdapter wraps a single cluster's cache operations
type clusterCacheAdapter struct {
	graphCache *GraphCache
	server     string
}

// GetServerVersion returns the Kubernetes server version
func (c *clusterCacheAdapter) GetServerVersion() string {
	info, err := c.graphCache.watchManager.GetDiscoveryClient().ServerVersion()
	if err != nil {
		log.Warnf("Failed to get server version: %v", err)
		return ""
	}
	return info.GitVersion
}

// GetAPIResources returns the list of API resources
func (c *clusterCacheAdapter) GetAPIResources() []kube.APIResourceInfo {
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
	// First check if we already watch this type (with proper locking)
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
func (c *clusterCacheAdapter) GetClusterInfo() cache.ClusterInfo {
	metrics := c.graphCache.GetMetrics()

	return cache.ClusterInfo{
		ResourcesCount: metrics.TotalManagedResources,
		APIsCount:      metrics.ActiveWatches,
		K8SVersion:     c.GetServerVersion(),
	}
}

// IterateHierarchy executes the callback for each resource in the hierarchy starting from the given key
func (c *clusterCacheAdapter) IterateHierarchy(key kube.ResourceKey, action func(child appv1.ResourceNode, appName string) bool) error {
	visited := make(map[kube.ResourceKey]bool)
	return c.iterateHierarchyRecursive(key, action, visited)
}

func (c *clusterCacheAdapter) iterateHierarchyRecursive(key kube.ResourceKey, action func(child appv1.ResourceNode, appName string) bool, visited map[kube.ResourceKey]bool) error {
	if visited[key] {
		return nil // Cycle detected, stop branch
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

// IterateHierarchyV2 iterates resource tree starting from the specified top level resources and executes callback for each resource in the tree
// Properly implements recursive traversal with cycle detection
func (c *clusterCacheAdapter) IterateHierarchyV2(keys []kube.ResourceKey, action func(resource *cache.Resource, namespaceResources map[kube.ResourceKey]*cache.Resource) bool) {
	visited := make(map[kube.ResourceKey]bool)

	var traverse func(key kube.ResourceKey)
	traverse = func(key kube.ResourceKey) {
		// Cycle detection
		if visited[key] {
			return
		}
		visited[key] = true

		node, exists := c.graphCache.graph.Get(key)
		if !exists {
			return
		}

		res := nodeToCacheResource(node)

		// Call action - if it returns false, stop traversing this branch
		if !action(res, nil) {
			return
		}

		// Recursively traverse children
		children := c.graphCache.graph.GetChildren(key)
		for _, child := range children {
			traverse(child.Key)
		}
	}

	// Traverse from each top-level key
	for _, key := range keys {
		traverse(key)
	}
}

// GetManagedLiveObjsForApp returns live objects managed by the application
// Returns objects from cache (no API calls) for performance
func (c *clusterCacheAdapter) GetManagedLiveObjsForApp(app *appv1.Application, targetObjs []*unstructured.Unstructured) (map[kube.ResourceKey]*unstructured.Unstructured, error) {
	appName := app.InstanceName(app.Namespace)
	resources := c.graphCache.GetResourcesByApplication(appName)
	result := make(map[kube.ResourceKey]*unstructured.Unstructured, len(resources))

	// Build map of target object keys for quick lookup
	targetKeys := make(map[kube.ResourceKey]bool, len(targetObjs))
	for _, obj := range targetObjs {
		targetKeys[kube.GetResourceKey(obj)] = true
	}

	// Return cached objects (no API calls!)
	for _, res := range resources {
		key := res.Key

		// Only return objects that are in the target list (if provided)
		if len(targetObjs) > 0 && !targetKeys[key] {
			continue
		}

		// Return the cached object
		if res.Resource != nil {
			// Deep copy to prevent external modifications
			result[key] = res.Resource.DeepCopy()
		} else {
			// Fallback: resource was added before we started storing full objects
			// This should be rare after initial cache population
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"key":       key,
			}).Warn("Resource in cache but full object not stored")
		}
	}

	return result, nil
}

// GetManagedLiveObjs returns managed objects
// Returns cached objects matching the isManaged predicate (no API calls)
func (c *clusterCacheAdapter) GetManagedLiveObjs(targetObjs []*unstructured.Unstructured, isManaged func(r *cache.Resource) bool) (map[kube.ResourceKey]*unstructured.Unstructured, error) {
	result := make(map[kube.ResourceKey]*unstructured.Unstructured)

	// Build map of target object keys for quick lookup
	targetKeys := make(map[kube.ResourceKey]bool)
	for _, obj := range targetObjs {
		targetKeys[kube.GetResourceKey(obj)] = true
	}

	// Get all nodes from the cache
	allNodes := c.graphCache.graph.GetAllNodes()

	for _, node := range allNodes {
		key := node.Key

		// Check if in target list (if provided)
		if len(targetObjs) > 0 && !targetKeys[key] {
			continue
		}

		// Check if managed using the provided predicate
		res := nodeToCacheResource(node)
		if isManaged != nil && !isManaged(res) {
			continue
		}

		// Return the cached object (no API call!)
		if node.Resource != nil {
			result[key] = node.Resource.DeepCopy()
		} else {
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"key":       key,
			}).Warn("Resource in cache but full object not stored")
		}
	}

	return result, nil
}

// IterateResources iterates over all resources in the cache
func (c *clusterCacheAdapter) IterateResources(callback func(res *cache.Resource, info *statecache.ResourceInfo)) error {
	allResources := c.graphCache.graph.GetAllNodes()

	for _, node := range allResources {
		resource := nodeToCacheResource(node)
		info := &statecache.ResourceInfo{
			AppName: node.ManagedBy,
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
			key := node.Key
			result[key] = nodeToResourceNode(node)
		}
	}

	return result, nil
}

// EnsureSynced checks if the cache is synced
func (c *clusterCacheAdapter) EnsureSynced() error {
	return nil
}

// GetOpenAPISchema returns the OpenAPI schema
func (c *clusterCacheAdapter) GetOpenAPISchema() openapi.Resources {
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

// GetGVKParser returns the GVK parser
func (c *clusterCacheAdapter) GetGVKParser() *managedfields.GvkParser {
	return nil
}

// Invalidate invalidates the cache
func (c *clusterCacheAdapter) Invalidate(opts ...cache.UpdateSettingsFunc) {
	go c.graphCache.DiscoverManagedResources()
}

// FindResources finds resources matching the given predicates
func (c *clusterCacheAdapter) FindResources(namespace string, predicates ...func(r *cache.Resource) bool) map[kube.ResourceKey]*cache.Resource {
	result := make(map[kube.ResourceKey]*cache.Resource)
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

func (c *clusterCacheAdapter) OnResourceUpdated(handler cache.OnResourceUpdatedHandler) cache.Unsubscribe {
	return func() {}
}

func (c *clusterCacheAdapter) OnEvent(handler cache.OnEventHandler) cache.Unsubscribe {
	return func() {}
}

func (c *clusterCacheAdapter) OnProcessEventsHandler(handler cache.OnProcessEventsHandler) cache.Unsubscribe {
	return func() {}
}
