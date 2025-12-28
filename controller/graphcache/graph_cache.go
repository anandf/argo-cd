package graphcache

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	repoclient "github.com/argoproj/argo-cd/v3/reposerver/apiclient"
)

// GraphCache is the main graph-based cache that watches only resources managed by Argo CD.
type GraphCache struct {
	// Core components
	graph               *ResourceGraph
	watchManager        *SelectiveWatchManager
	descendantTracker   *DescendantTracker
	manifestDiscovery   *ManifestDiscovery
	typeRelationships   *TypeRelationshipCache
	cyphernetesExecutor *CyphernetesQueryExecutor

	// Configuration
	trackingMethod TrackingMethod
	namespaces     []string

	// Discovery state
	discoveredTypes map[schema.GroupKind]bool
	discoveryLock   sync.RWMutex

	// Context
	ctx    context.Context
	cancel context.CancelFunc

	// Metrics
	metricsLock sync.RWMutex
	metrics     CacheMetrics
}

// CacheMetrics contains comprehensive metrics about the graph cache.
type CacheMetrics struct {
	// Resource counts
	TotalManagedResources  int
	ResourcesByType        map[schema.GroupKind]int
	ResourcesByApplication map[string]int

	// Watch counts
	ActiveWatches int
	WatchesByType map[schema.GroupKind]bool

	// Discovery metrics
	DiscoveryRuns         int
	LastDiscoveryTime     time.Time
	LastDiscoveryDuration time.Duration
	ResourcesDiscovered   int
	DescendantTypesAdded  int

	// Event metrics
	TotalEvents  int64
	AddEvents    int64
	UpdateEvents int64
	DeleteEvents int64

	// Performance
	AverageEventProcessTime time.Duration
}

// Config contains configuration for the graph cache.
type Config struct {
	DynamicClient    dynamic.Interface
	DiscoveryClient  discovery.DiscoveryInterface
	RepoServerClient repoclient.RepoServerServiceClient
	TrackingMethod   TrackingMethod
	Namespaces       []string
}

// NewGraphCache creates a new graph-based cache.
func NewGraphCache(config Config) (*GraphCache, error) {
	ctx, cancel := context.WithCancel(context.Background())

	gc := &GraphCache{
		graph:             NewResourceGraph(),
		descendantTracker: NewDescendantTracker(),
		typeRelationships: NewTypeRelationshipCache(),
		trackingMethod:    config.TrackingMethod,
		namespaces:        config.Namespaces,
		discoveredTypes:   make(map[schema.GroupKind]bool),
		ctx:               ctx,
		cancel:            cancel,
		metrics: CacheMetrics{
			ResourcesByType:        make(map[schema.GroupKind]int),
			ResourcesByApplication: make(map[string]int),
			WatchesByType:          make(map[schema.GroupKind]bool),
		},
	}

	// Create watch manager with event handler
	gc.watchManager = NewSelectiveWatchManager(
		config.DynamicClient,
		config.DiscoveryClient,
		config.TrackingMethod,
		config.Namespaces,
		gc.handleResourceEvent,
	)

	// Create manifest discovery if repo server client is provided
	if config.RepoServerClient != nil {
		gc.manifestDiscovery = NewManifestDiscovery(ManifestDiscoveryConfig{
			RepoServerClient:  config.RepoServerClient,
			TypeRelationships: gc.typeRelationships,
			GraphCache:        gc,
			CacheTTL:          5 * time.Minute,
		})
	}

	// Initialize Cyphernetes executor
	cqe, err := NewCyphernetesQueryExecutor(gc, config.DynamicClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create cyphernetes executor: %w", err)
	}
	gc.cyphernetesExecutor = cqe

	return gc, nil
}

// Start begins the graph cache operation by performing initial discovery.
func (gc *GraphCache) Start() error {
	log.WithField("component", "graph-cache").Info("Starting graph cache")

	// Perform initial discovery
	if err := gc.DiscoverManagedResources(); err != nil {
		return fmt.Errorf("initial discovery failed: %w", err)
	}

	// Start periodic discovery (every 5 minutes)
	go gc.periodicDiscovery()

	log.WithFields(log.Fields{
		"component":         "graph-cache",
		"managed_resources": gc.graph.Size(),
		"active_watches":    len(gc.watchManager.GetActiveWatches()),
	}).Info("Graph cache started successfully")

	return nil
}

// Shutdown stops the graph cache and cleans up resources.
func (gc *GraphCache) Shutdown() {
	log.WithField("component", "graph-cache").Info("Shutting down graph cache")

	gc.cancel()
	gc.watchManager.Shutdown()

	log.WithField("component", "graph-cache").Info("Graph cache shutdown complete")
}

// DiscoverManagedResources discovers all resources managed by Argo CD and sets up watches.
func (gc *GraphCache) DiscoverManagedResources() error {
	startTime := time.Now()
	defer func() {
		gc.metricsLock.Lock()
		gc.metrics.DiscoveryRuns++
		gc.metrics.LastDiscoveryTime = time.Now()
		gc.metrics.LastDiscoveryDuration = time.Since(startTime)
		gc.metricsLock.Unlock()
	}()

	log.WithField("component", "graph-cache").Info("Starting resource discovery")

	// Well-known resource types that typically have Argo CD managed resources
	commonTypes := []schema.GroupKind{
		{Group: "apps", Kind: "Deployment"},
		{Group: "apps", Kind: "StatefulSet"},
		{Group: "apps", Kind: "DaemonSet"},
		{Group: "apps", Kind: "ReplicaSet"},
		{Group: "", Kind: "Service"},
		{Group: "", Kind: "ConfigMap"},
		{Group: "", Kind: "Secret"},
		{Group: "", Kind: "Pod"},
		{Group: "batch", Kind: "Job"},
		{Group: "batch", Kind: "CronJob"},
		{Group: "networking.k8s.io", Kind: "Ingress"},
		{Group: "", Kind: "ServiceAccount"},
	}

	totalDiscovered := 0

	for _, gk := range commonTypes {
		count, err := gc.discoverResourceType(gk)
		if err != nil {
			log.WithError(err).WithFields(log.Fields{
				"component": "graph-cache",
				"group":     gk.Group,
				"kind":      gk.Kind,
			}).Warn("Failed to discover resource type")
			continue
		}

		totalDiscovered += count

		if count > 0 {
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"group":     gk.Group,
				"kind":      gk.Kind,
				"count":     count,
			}).Debug("Discovered managed resources")
		}
	}

	gc.metricsLock.Lock()
	gc.metrics.ResourcesDiscovered = totalDiscovered
	gc.metricsLock.Unlock()

	log.WithFields(log.Fields{
		"component":   "graph-cache",
		"resources":   totalDiscovered,
		"types":       len(gc.discoveredTypes),
		"duration_ms": time.Since(startTime).Milliseconds(),
	}).Info("Resource discovery complete")

	return nil
}

// discoverResourceType discovers resources of a specific type and adds them to the graph.
func (gc *GraphCache) discoverResourceType(gk schema.GroupKind) (int, error) {
	// Get GVR and scope
	gvr, isNamespaced, err := gc.watchManager.discoverResource(gk)
	if err != nil {
		return 0, err
	}

	// List managed resources
	resources, err := gc.watchManager.ListManagedResources(gvr, isNamespaced)
	if err != nil {
		return 0, err
	}

	if len(resources) == 0 {
		return 0, nil
	}

	// Add resources to graph
	for _, obj := range resources {
		gc.addResourceToGraph(obj)
	}

	// Mark type as discovered
	gc.discoveryLock.Lock()
	gc.discoveredTypes[gk] = true
	gc.discoveryLock.Unlock()

	// Add Cyphernetes rule for this type
	if gc.cyphernetesExecutor != nil {
		gc.cyphernetesExecutor.AddRuleForResourceKind(gk.Kind, string(gc.trackingMethod))
	}

	// Ensure watch exists for this type
	created, err := gc.watchManager.EnsureWatch(gk)
	if err != nil {
		return len(resources), fmt.Errorf("failed to create watch: %w", err)
	}

	if created {
		gc.metricsLock.Lock()
		gc.metrics.WatchesByType[gk] = true
		gc.metricsLock.Unlock()
	}

	// Discover and watch descendant types
	gc.ensureDescendantWatches(gk)

	return len(resources), nil
}

// addResourceToGraph adds a resource to the graph and updates indices.
func (gc *GraphCache) addResourceToGraph(obj *unstructured.Unstructured) {
	// Extract tracking information
	trackingInfo := ExtractTrackingInfo(obj, gc.trackingMethod)
	if !trackingInfo.HasTracking {
		return
	}

	// Extract parent references
	parents := gc.descendantTracker.ExtractParentReferences(obj)

	// Create resource node
	node := &ResourceNode{
		Key:             ToResourceKey(obj),
		ResourceVersion: obj.GetResourceVersion(),
		ManagedBy:       trackingInfo.AppName,
		TrackingID:      trackingInfo.TrackingID,
		Parents:         parents,
		Children:        []kube.ResourceKey{}, // Will be populated by graph
		Info: &ResourceMetadata{
			Labels:      obj.GetLabels(),
			Annotations: obj.GetAnnotations(),
			OwnerRefs:   obj.GetOwnerReferences(),
		},
	}

	// Add to graph
	gc.graph.AddOrUpdate(node)

	log.WithFields(log.Fields{
		"component": "graph-cache",
		"app":       trackingInfo.AppName,
		"kind":      node.Key.Kind,
		"namespace": node.Key.Namespace,
		"name":      node.Key.Name,
	}).Debug("Added resource to graph")
}

// ensureDescendantWatches ensures watches exist for descendant resource types.
func (gc *GraphCache) ensureDescendantWatches(parentGK schema.GroupKind) {
	descendants := gc.descendantTracker.GetExpectedDescendants(parentGK)

	for _, childGK := range descendants {
		// Check if already discovered
		gc.discoveryLock.RLock()
		discovered := gc.discoveredTypes[childGK]
		gc.discoveryLock.RUnlock()

		if discovered {
			continue
		}

		// Try to create watch for descendant type
		created, err := gc.watchManager.EnsureWatch(childGK)
		if err != nil {
			log.WithError(err).WithFields(log.Fields{
				"component": "graph-cache",
				"group":     childGK.Group,
				"kind":      childGK.Kind,
			}).Debug("Failed to create descendant watch")
			continue
		}

		if created {
			gc.discoveryLock.Lock()
			gc.discoveredTypes[childGK] = true
			gc.discoveryLock.Unlock()

			// Add Cyphernetes rule for this type
			if gc.cyphernetesExecutor != nil {
				gc.cyphernetesExecutor.AddRuleForResourceKind(childGK.Kind, string(gc.trackingMethod))
			}

			gc.metricsLock.Lock()
			gc.metrics.DescendantTypesAdded++
			gc.metrics.WatchesByType[childGK] = true
			gc.metricsLock.Unlock()

			log.WithFields(log.Fields{
				"component": "graph-cache",
				"parent":    parentGK.Kind,
				"child":     childGK.Kind,
			}).Debug("Added descendant watch")
		}
	}
}

// handleResourceEvent handles watch events for resources.
func (gc *GraphCache) handleResourceEvent(eventType watch.EventType, obj *unstructured.Unstructured) {
	startTime := time.Now()
	defer func() {
		gc.metricsLock.Lock()
		gc.metrics.TotalEvents++
		// Update average processing time (simple moving average)
		if gc.metrics.AverageEventProcessTime == 0 {
			gc.metrics.AverageEventProcessTime = time.Since(startTime)
		} else {
			gc.metrics.AverageEventProcessTime = (gc.metrics.AverageEventProcessTime + time.Since(startTime)) / 2
		}
		gc.metricsLock.Unlock()
	}()

	switch eventType {
	case watch.Added, watch.Modified:
		gc.addResourceToGraph(obj)

		gc.metricsLock.Lock()
		if eventType == watch.Added {
			gc.metrics.AddEvents++
		} else {
			gc.metrics.UpdateEvents++
		}
		gc.metricsLock.Unlock()

		// Check if this is a new resource type
		gk := ToGroupKind(obj)
		gc.discoveryLock.RLock()
		discovered := gc.discoveredTypes[gk]
		gc.discoveryLock.RUnlock()

		if !discovered {
			gc.discoveryLock.Lock()
			gc.discoveredTypes[gk] = true
			gc.discoveryLock.Unlock()

			// Add Cyphernetes rule for this type
			if gc.cyphernetesExecutor != nil {
				gc.cyphernetesExecutor.AddRuleForResourceKind(gk.Kind, string(gc.trackingMethod))
			}

			// Ensure descendant watches
			gc.ensureDescendantWatches(gk)
		}

	case watch.Deleted:
		key := ToResourceKey(obj)
		gc.graph.Delete(key)

		gc.metricsLock.Lock()
		gc.metrics.DeleteEvents++
		gc.metricsLock.Unlock()

		log.WithFields(log.Fields{
			"component": "graph-cache",
			"kind":      key.Kind,
			"namespace": key.Namespace,
			"name":      key.Name,
		}).Debug("Deleted resource from graph")
	}
}

// periodicDiscovery runs discovery periodically to catch new resources.
func (gc *GraphCache) periodicDiscovery() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-gc.ctx.Done():
			return
		case <-ticker.C:
			if err := gc.DiscoverManagedResources(); err != nil {
				log.WithError(err).WithField("component", "graph-cache").Warn("Periodic discovery failed")
			}
		}
	}
}

// GetDynamicClient returns the dynamic client used by the cache
func (gc *GraphCache) GetDynamicClient() dynamic.Interface {
	return gc.watchManager.GetDynamicClient()
}

// GetMetrics returns current cache metrics.
func (gc *GraphCache) GetMetrics() CacheMetrics {
	gc.metricsLock.Lock()
	defer gc.metricsLock.Unlock()

	// Get graph metrics
	graphMetrics := gc.graph.GetMetrics()

	// Get watch metrics
	watchMetrics := gc.watchManager.GetMetrics()

	// Combine metrics
	gc.metrics.TotalManagedResources = graphMetrics.TotalResources
	gc.metrics.ResourcesByType = graphMetrics.ResourcesByType
	gc.metrics.ResourcesByApplication = graphMetrics.ResourcesByApp
	gc.metrics.ActiveWatches = watchMetrics.ActiveWatches

	// Deep copy for safety
	metrics := gc.metrics
	metrics.ResourcesByType = make(map[schema.GroupKind]int)
	metrics.ResourcesByApplication = make(map[string]int)
	metrics.WatchesByType = make(map[schema.GroupKind]bool)

	for k, v := range gc.metrics.ResourcesByType {
		metrics.ResourcesByType[k] = v
	}

	for k, v := range gc.metrics.ResourcesByApplication {
		metrics.ResourcesByApplication[k] = v
	}

	for k, v := range gc.metrics.WatchesByType {
		metrics.WatchesByType[k] = v
	}

	return metrics
}

// GetResourcesByApplication returns all resources managed by an application.
func (gc *GraphCache) GetResourcesByApplication(appName string) []*ResourceNode {
	return gc.graph.GetByApplication(appName)
}

// GetByType returns all resources of a specific GroupKind.
func (gc *GraphCache) GetByType(gk schema.GroupKind) []*ResourceNode {
	return gc.graph.GetByType(gk)
}

// GetByLabel returns all resources matching a label key and value.
func (gc *GraphCache) GetByLabel(key, value string) []*ResourceNode {
	return gc.graph.GetByLabel(key, value)
}

// GetAllTypes returns all resource types in the cache.
func (gc *GraphCache) GetAllTypes() []schema.GroupKind {
	return gc.graph.GetAllTypes()
}

// GetAllNodes returns all nodes in the cache.
func (gc *GraphCache) GetAllNodes() []*ResourceNode {
	return gc.graph.GetAllNodes()
}

// GetResource retrieves a specific resource from the cache.
func (gc *GraphCache) GetResource(key kube.ResourceKey) (*ResourceNode, bool) {
	return gc.graph.Get(key)
}

// GetChildren returns all children of a resource.
func (gc *GraphCache) GetChildren(key kube.ResourceKey) []*ResourceNode {
	return gc.graph.GetChildren(key)
}

// GetParents returns all parents of a resource.
func (gc *GraphCache) GetParents(key kube.ResourceKey) []*ResourceNode {
	return gc.graph.GetParents(key)
}

// GetAllApplications returns all application names in the cache.
func (gc *GraphCache) GetAllApplications() []string {
	return gc.graph.GetAllApplications()
}

// GetAllResourceTypes returns all resource types in the cache.
func (gc *GraphCache) GetAllResourceTypes() []schema.GroupKind {
	return gc.graph.GetAllTypes()
}

// Size returns the total number of resources in the cache.
func (gc *GraphCache) Size() int {
	return gc.graph.Size()
}

// PrepareForApplication performs manifest-based discovery for an application
// This should be called before syncing an application to ensure watches are in place
func (gc *GraphCache) PrepareForApplication(ctx context.Context, app *appv1.Application) error {
	if gc.manifestDiscovery == nil {
		log.WithField("component", "graph-cache").
			Warn("Manifest discovery not available (no repo server client)")
		return nil
	}

	return gc.manifestDiscovery.DiscoverFromApplication(ctx, app)
}

// Snapshot creates a serializable snapshot of the current graph state
func (gc *GraphCache) Snapshot() (*GraphSnapshot, error) {
	nodes := gc.graph.GetAllNodes()
	snapshotNodes := make([]SnapshotNode, len(nodes))

	for i, node := range nodes {
		snapshotNodes[i] = SnapshotNode{
			Key:             node.Key,
			Version:         node.Version,
			UID:             node.UID,
			ResourceVersion: node.ResourceVersion,
			ManagedBy:       node.ManagedBy,
			TrackingID:      node.TrackingID,
			Parents:         node.Parents,
			Info:            node.Info,
			CreatedAt:       node.CreatedAt,
		}
	}

	return &GraphSnapshot{
		Version:     "v1",
		LastUpdated: time.Now(),
		Nodes:       snapshotNodes,
	}, nil
}

// Restore populates the graph from a snapshot
func (gc *GraphCache) Restore(snapshot *GraphSnapshot) error {
	startTime := time.Now()
	log.WithField("nodes", len(snapshot.Nodes)).Info("Restoring graph from snapshot")

	for _, sNode := range snapshot.Nodes {
		node := &ResourceNode{
			Key:             sNode.Key,
			Version:         sNode.Version,
			UID:             sNode.UID,
			ResourceVersion: sNode.ResourceVersion,
			ManagedBy:       sNode.ManagedBy,
			TrackingID:      sNode.TrackingID,
			Parents:         sNode.Parents,
			Info:            sNode.Info,
			CreatedAt:       sNode.CreatedAt,
		}
		gc.graph.AddOrUpdate(node)
	}

	log.WithFields(log.Fields{
		"duration": time.Since(startTime),
		"nodes":    len(snapshot.Nodes),
	}).Info("Graph restoration complete")
	return nil
}

// LearnResourceRelationships learns parent-child relationships from actual resources
// This is called when resources are observed in the cluster to build the type relationship cache
func (gc *GraphCache) LearnResourceRelationships(parent, child *unstructured.Unstructured) {
	parentGVK := parent.GroupVersionKind()
	childGVK := child.GroupVersionKind()

	// Learn this relationship with confidence level 1 (observed once)
	gc.typeRelationships.LearnRelationship(parentGVK, childGVK, 1)

	log.WithFields(log.Fields{
		"component": "graph-cache",
		"parent":    parentGVK.String(),
		"child":     childGVK.String(),
	}).Debug("Learned resource relationship")
}

// EnsureWatch creates a watch for a specific GVK and namespace
// This method is called by ManifestDiscovery to create watches proactively
func (gc *GraphCache) EnsureWatch(gvk schema.GroupVersionKind, namespace string) error {
	gk := schema.GroupKind{
		Group: gvk.Group,
		Kind:  gvk.Kind,
	}

	// Check if already discovered
	gc.discoveryLock.RLock()
	discovered := gc.discoveredTypes[gk]
	gc.discoveryLock.RUnlock()

	if discovered {
		return nil // Already watching
	}

	// Create the watch
	created, err := gc.watchManager.EnsureWatch(gk)
	if err != nil {
		return fmt.Errorf("failed to create watch for %s: %w", gvk.String(), err)
	}

	if created {
		gc.discoveryLock.Lock()
		gc.discoveredTypes[gk] = true
		gc.discoveryLock.Unlock()

		gc.metricsLock.Lock()
		gc.metrics.WatchesByType[gk] = true
		gc.metricsLock.Unlock()

		log.WithFields(log.Fields{
			"component": "graph-cache",
			"gvk":       gvk.String(),
			"namespace": namespace,
		}).Info("Created watch for resource type")
	}

	return nil
}

// GetTypeRelationships returns the type relationship cache for inspection
func (gc *GraphCache) GetTypeRelationships() *TypeRelationshipCache {
	return gc.typeRelationships
}

// InvalidateManifestCache invalidates the manifest cache for an application
// This should be called when an application's source changes
func (gc *GraphCache) InvalidateManifestCache(appNamespace, appName string) {
	if gc.manifestDiscovery != nil {
		gc.manifestDiscovery.InvalidateCache(appNamespace, appName)
	}
}
