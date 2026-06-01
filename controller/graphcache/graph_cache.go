package graphcache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2/textlogger"

	graphcore "github.com/argoproj/argo-cd/gitops-engine/pkg/graphcache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	statecache "github.com/argoproj/argo-cd/v3/controller/cache"
	"github.com/argoproj/argo-cd/v3/controller/metrics"
	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	repoclient "github.com/argoproj/argo-cd/v3/reposerver/apiclient"
)

// GraphCache is the main graph-based cache that watches only resources managed by Argo CD.
type GraphCache struct {
	// Core components
	graph             *graphcore.ResourceGraph
	watchManager      *graphcore.SelectiveWatchManager
	descendantTracker *graphcore.DescendantTracker
	manifestDiscovery *ManifestDiscovery
	typeRelationships *graphcore.TypeRelationshipCache

	// Configuration
	trackingMethod      graphcore.TrackingMethod
	namespaces          []string
	serverURL           string               // For metrics labeling
	graphConfig         graphcore.GraphConfig // Tunable parameters
	appInstanceLabelKey string               // Custom tracking label key (default: app.kubernetes.io/instance)
	configLock          sync.RWMutex         // Protects mutable config fields like appInstanceLabelKey

	// Discovery state
	discoveredTypes  map[schema.GroupKind]bool
	pendingWatchGVKs map[schema.GroupVersionKind]string // GVK → namespace for watches that failed (e.g., CRD not yet created)
	discoveryLock    sync.RWMutex

	// Context
	ctx    context.Context
	cancel context.CancelFunc

	// Callbacks for adapter integration
	onResourceUpdated           func(newRes, oldRes *graphcore.ResourceNode, obj *unstructured.Unstructured, eventType watch.EventType)
	populateResourceInfoHandler PopulateResourceInfoHandler

	// Persistence
	relationshipStore graphcore.RelationshipStore

	// Metrics
	metricsLock       sync.RWMutex
	metrics           CacheMetrics
	prometheusMetrics *metrics.GraphCacheMetrics
}

// CacheMetrics contains comprehensive metrics about the graph cache.
type CacheMetrics struct {
	// Resource counts
	TotalManagedResources   int
	ResourcesByType         map[schema.GroupKind]int
	ResourcesByApplication  map[string]int
	UniqueApplications      int
	UniqueResourceTypes     int

	// Watch counts
	ActiveWatches           int
	WatchesByType           map[schema.GroupKind]bool

	// Discovery metrics
	DiscoveryRuns           int
	LastDiscoveryTime       time.Time
	LastDiscoveryDuration   time.Duration
	ResourcesDiscovered     int
	DescendantTypesAdded    int

	// Event metrics
	TotalEvents             int64
	AddEvents               int64
	UpdateEvents            int64
	DeleteEvents            int64
	SkippedEvents           int64 // Events skipped by pre-filter (unmanaged resources)

	// Performance
	AverageEventProcessTime time.Duration
}

// PopulateResourceInfoHandler is called to populate enrichment info (health, images,
// networking) for each resource. Matches the signature used by gitops-engine.
// Returns the info object and whether the full manifest should be cached.
type PopulateResourceInfoHandler func(un *unstructured.Unstructured, isRoot bool) (info interface{}, cacheManifest bool)

// Config contains configuration for the graph cache.
type Config struct {
	DynamicClient    dynamic.Interface
	DiscoveryClient  discovery.DiscoveryInterface
	RepoServerClient repoclient.RepoServerServiceClient
	TrackingMethod   graphcore.TrackingMethod
	Namespaces       []string
	ServerURL        string               // Server URL for metrics labeling (optional)
	GraphConfig      graphcore.GraphConfig // Tunable parameters (optional, uses defaults if not set)

	// PopulateResourceInfoHandler is called for each resource to compute
	// health status, images, networking info, etc. If nil, no enrichment is done.
	PopulateResourceInfoHandler PopulateResourceInfoHandler

	// PrometheusRegistry is the Prometheus registry to register graph cache metrics with.
	// If nil, metrics are not registered with any registry.
	PrometheusRegistry *prometheus.Registry

	// RelationshipStore persists learned type relationships across restarts.
	// If nil, learned relationships are not persisted.
	RelationshipStore graphcore.RelationshipStore

	// CustomRelationshipsFile is the path to a YAML file defining custom
	// parent→child type relationships. If empty, no custom relationships are loaded.
	CustomRelationshipsFile string
}

// NewGraphCache creates a new graph-based cache.
// The provided context will be used for all operations and watch management.
func NewGraphCache(ctx context.Context, config Config) (*GraphCache, error) {
	// Validate required configuration
	if config.DynamicClient == nil {
		return nil, fmt.Errorf("DynamicClient is required")
	}
	if config.DiscoveryClient == nil {
		return nil, fmt.Errorf("DiscoveryClient is required")
	}
	if config.TrackingMethod == "" {
		return nil, fmt.Errorf("TrackingMethod is required")
	}

	// Validate tracking method value
	validMethods := map[graphcore.TrackingMethod]bool{
		graphcore.TrackingMethodLabel:              true,
		graphcore.TrackingMethodAnnotation:         true,
		graphcore.TrackingMethodAnnotationAndLabel: true,
	}
	if !validMethods[config.TrackingMethod] {
		return nil, fmt.Errorf("invalid TrackingMethod: %s", config.TrackingMethod)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)

	// Use default config if not provided
	graphConfig := config.GraphConfig
	if graphConfig.DiscoveryInterval == 0 {
		graphConfig = graphcore.DefaultGraphConfig()
	}

	gc := &GraphCache{
		graph:             graphcore.NewResourceGraph(),
		descendantTracker: graphcore.NewDescendantTracker(),
		typeRelationships: graphcore.NewTypeRelationshipCache(),
		trackingMethod:   config.TrackingMethod,
		namespaces:       config.Namespaces,
		serverURL:        config.ServerURL,
		graphConfig:      graphConfig,
		discoveredTypes:  make(map[schema.GroupKind]bool),
		pendingWatchGVKs: make(map[schema.GroupVersionKind]string),
		ctx:               ctx,
		cancel:            cancel,
		metrics: CacheMetrics{
			ResourcesByType:        make(map[schema.GroupKind]int),
			ResourcesByApplication: make(map[string]int),
			WatchesByType:          make(map[schema.GroupKind]bool),
		},
		prometheusMetrics:           metrics.NewGraphCacheMetrics(config.PrometheusRegistry),
		populateResourceInfoHandler: config.PopulateResourceInfoHandler,
	}

	// Load custom relationships from file
	if config.CustomRelationshipsFile != "" {
		customRels, err := graphcore.LoadCustomRelationships(textlogger.NewLogger(textlogger.NewConfig()).WithName("graph-cache"), config.CustomRelationshipsFile)
		if err != nil {
			log.WithError(err).Warn("Failed to load custom relationships file")
		} else if len(customRels) > 0 {
			for _, rel := range customRels {
				gc.typeRelationships.LearnRelationship(rel.Parent, rel.Child)
			}
			log.WithField("count", len(customRels)).Info("Loaded custom type relationships from file")
		}
	}

	// Restore persisted relationships from store
	if config.RelationshipStore != nil {
		gc.relationshipStore = config.RelationshipStore
		persisted, err := config.RelationshipStore.Load()
		if err != nil {
			log.WithError(err).Warn("Failed to load persisted relationships")
		} else if len(persisted) > 0 {
			for _, rel := range persisted {
				parentGVK, pErr := graphcore.ParseGVKString(rel.Parent)
				if pErr != nil {
					continue
				}
				childGVK, cErr := graphcore.ParseGVKString(rel.Child)
				if cErr != nil {
					continue
				}
				gc.typeRelationships.LearnRelationship(parentGVK, childGVK)
			}
			log.WithField("count", len(persisted)).Info("Restored persisted type relationships")
		}
	}

	// Create watch manager with event handler, derived from graph cache context
	gc.watchManager = graphcore.NewSelectiveWatchManager(
		ctx,
		config.DynamicClient,
		config.DiscoveryClient,
		config.TrackingMethod,
		config.Namespaces,
		gc.handleResourceEvent,
		graphConfig,
		textlogger.NewLogger(textlogger.NewConfig()).WithName("graph-cache"),
	)

	// Create manifest discovery if repo server client is provided
	if config.RepoServerClient != nil {
		gc.manifestDiscovery = NewManifestDiscovery(ManifestDiscoveryConfig{
			RepoServerClient:  config.RepoServerClient,
			TypeRelationships: gc.typeRelationships,
			GraphCache:        gc,
		})
	}

	return gc, nil
}

// Start begins the graph cache operation by performing initial discovery.
func (gc *GraphCache) Start() error {
	log.WithField("component", "graph-cache").Info("Starting graph cache")

	// Perform initial discovery
	if err := gc.DiscoverManagedResources(); err != nil {
		log.WithError(err).Error("Initial discovery failed")
		return fmt.Errorf("initial discovery failed: %w", err)
	}

	// Start periodic discovery (every 5 minutes)
	go gc.periodicDiscovery()

	// Start periodic metrics export (every 30 seconds)
	go gc.periodicMetricsExport()

	// Start periodic relationship persistence
	if gc.relationshipStore != nil {
		go gc.periodicRelationshipPersistence()
	}

	log.WithFields(log.Fields{
		"managed_resources": gc.graph.Size(),
		"active_watches":    len(gc.watchManager.GetActiveWatches()),
	}).Info("Graph cache started successfully")

	return nil
}

// Shutdown stops the graph cache and cleans up resources.
func (gc *GraphCache) Shutdown() {
	log.WithField("component", "graph-cache").Info("Shutting down graph cache")

	if gc.relationshipStore != nil {
		if err := gc.saveRelationships(); err != nil {
			log.WithError(err).Warn("Failed to save relationships on shutdown")
		}
	}

	gc.cancel()
	gc.watchManager.Shutdown()

	log.WithField("component", "graph-cache").Info("Graph cache shutdown complete")
}

// SetTrackingMethod updates the tracking method and re-evaluates all existing resources.
// This is called when the tracking method setting changes at runtime (e.g., switching
// from annotation to label tracking). All resources in the graph are re-processed
// to update their ManagedBy field according to the new tracking method.
func (gc *GraphCache) SetTrackingMethod(method graphcore.TrackingMethod) {
	gc.configLock.Lock()
	gc.trackingMethod = method
	gc.configLock.Unlock()

	// Re-process all existing resources with the new tracking method
	nodes := gc.graph.GetAllNodes()
	for _, node := range nodes {
		if node.Resource != nil {
			gc.addResourceToGraph(node.Resource)
		}
	}

	log.WithFields(log.Fields{
		"component":      "graph-cache",
		"trackingMethod": method,
		"reprocessed":    len(nodes),
	}).Info("Tracking method updated, re-processed all resources")
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

	// Well-known resource types that typically have Argo CD managed resources.
	// CRDs are included so that CRD add/modify events trigger watches for
	// custom resource types whose CRD didn't exist at manifest-discovery time.
	commonTypes := []schema.GroupKind{
		{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"},
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

	gc.discoveryLock.RLock()
	discoveredCount := len(gc.discoveredTypes)
	gc.discoveryLock.RUnlock()

	log.WithFields(log.Fields{
		"component": "graph-cache",
		"resources": totalDiscovered,
		"types":     discoveredCount,
	}).Info("Resource discovery complete")

	return nil
}

// discoverResourceType discovers resources of a specific type and adds them to the graph.
func (gc *GraphCache) discoverResourceType(gk schema.GroupKind) (int, error) {
	// Get GVR and scope
	gvr, isNamespaced, err := gc.watchManager.DiscoverResource(gk)
	if err != nil {
		return 0, err
	}

	// List managed resources and capture ResourceVersion for consistent watch
	resources, resourceVersion, err := gc.watchManager.ListManagedResources(gvr, isNamespaced)
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

	// Ensure watch exists starting from the list's ResourceVersion to avoid missing events
	created, err := gc.watchManager.EnsureWatch(gk, "", resourceVersion)
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
// Resources without Argo CD tracking labels are still accepted — their app ownership
// is derived from the parent chain via OwnerReferences. This allows child resources
// (ReplicaSets, Pods) to be properly associated with their parent application.
func (gc *GraphCache) addResourceToGraph(obj *unstructured.Unstructured) {
	// Pre-filter: skip resources that are clearly not managed by Argo CD.
	// This avoids expensive operations (DeepCopy, health checks, graph updates)
	// on unmanaged resources that flow through watches without label selectors.
	key := graphcore.ToResourceKey(obj)
	_, existsInGraph := gc.graph.Get(key)
	if !existsInGraph && !gc.hasTrackingMarkers(obj) {
		return
	}

	// Extract tracking information
	trackingInfo := graphcore.ExtractTrackingInfo(obj, gc.getTrackingMethod())

	// Extract parent references
	parents := gc.descendantTracker.ExtractParentReferences(obj)

	// Deep copy the object to avoid external modifications
	objCopy := obj.DeepCopy()

	// Create resource node with full object
	gvk := obj.GroupVersionKind()
	node := &graphcore.ResourceNode{
		Key:             graphcore.ToResourceKey(obj),
		Version:         gvk.Version,
		UID:             string(obj.GetUID()),
		ResourceVersion: obj.GetResourceVersion(),
		ManagedBy:       trackingInfo.AppName, // May be empty for untracked resources
		TrackingID:      trackingInfo.TrackingID,
		Parents:         parents,
		Children:        []kube.ResourceKey{}, // Will be populated by graph
		Info: &graphcore.ResourceMetadata{
			Labels:      obj.GetLabels(),
			Annotations: obj.GetAnnotations(),
			OwnerRefs:   obj.GetOwnerReferences(),
		},
		Resource: objCopy, // Store full object for cache hits
	}

	// Call PopulateResourceInfoHandler if configured.
	// The handler uses resourceTracking.GetAppName() which correctly handles
	// custom tracking labels (application.instanceLabelKey), annotation tracking,
	// and installation ID — unlike the internal ExtractTrackingInfo which only
	// knows about the hardcoded label. Use the handler's AppName as source of truth.
	if gc.populateResourceInfoHandler != nil {
		isRoot := len(parents) == 0
		info, cacheManifest := gc.populateResourceInfoHandler(obj, isRoot)
		node.CachedInfo = info
		if !cacheManifest {
			node.Resource = nil // Don't store full manifest if handler says no
		}
		// Use AppName from handler as authoritative source of tracking info.
		// The handler calls resourceTracking.GetAppName() which correctly handles
		// custom tracking labels (application.instanceLabelKey) and annotation tracking,
		// unlike the internal ExtractTrackingInfo which only knows hardcoded labels.
		if ri, ok := info.(*statecache.ResourceInfo); ok && ri != nil && ri.AppName != "" {
			node.ManagedBy = ri.AppName
			trackingInfo.HasTracking = true
			trackingInfo.AppName = ri.AppName
		}
	}

	// Add to graph
	gc.graph.AddOrUpdate(node)

	// If resource has no tracking, try to derive app name from parent chain
	if !trackingInfo.HasTracking {
		appName := gc.deriveAppNameFromParents(node.Key)
		if appName != "" {
			gc.updateNodeAppName(node.Key, appName)
		}
	} else {
		// Resource has tracking — propagate app name to children that lack tracking
		gc.propagateAppNameToChildren(node.Key, trackingInfo.AppName)
	}

	log.WithFields(log.Fields{
		"component": "graph-cache",
		"app":       node.ManagedBy,
		"kind":      node.Key.Kind,
		"namespace": node.Key.Namespace,
		"name":      node.Key.Name,
		"tracked":   trackingInfo.HasTracking,
	}).Debug("Added resource to graph")
}

// deriveAppNameFromParents walks the parent chain via OwnerReferences to find
// the nearest ancestor with a ManagedBy value. Uses a depth limit to prevent cycles.
func (gc *GraphCache) deriveAppNameFromParents(key kube.ResourceKey) string {
	return gc.deriveAppNameRecursive(key, 0, make(map[kube.ResourceKey]bool))
}

func (gc *GraphCache) deriveAppNameRecursive(key kube.ResourceKey, depth int, visited map[kube.ResourceKey]bool) string {
	if depth > 10 || visited[key] {
		return ""
	}
	visited[key] = true

	node, exists := gc.graph.Get(key)
	if !exists {
		return ""
	}

	if node.ManagedBy != "" {
		return node.ManagedBy
	}

	for _, parent := range node.Parents {
		appName := gc.deriveAppNameRecursive(parent.ResourceKey, depth+1, visited)
		if appName != "" {
			return appName
		}
	}

	return ""
}

// updateNodeAppName updates the ManagedBy field for a node and fixes the app index.
func (gc *GraphCache) updateNodeAppName(key kube.ResourceKey, appName string) {
	gc.graph.UpdateNodeAppName(key, appName)
}

// propagateAppNameToChildren propagates app ownership to descendant resources
// that currently have no ManagedBy value. Handles the race condition where
// children arrive in the graph before their parents.
// Uses a visited set to prevent infinite recursion on graph cycles.
func (gc *GraphCache) propagateAppNameToChildren(key kube.ResourceKey, appName string) {
	if appName == "" {
		return
	}
	gc.propagateAppNameRecursive(key, appName, make(map[kube.ResourceKey]bool))
}

func (gc *GraphCache) propagateAppNameRecursive(key kube.ResourceKey, appName string, visited map[kube.ResourceKey]bool) {
	if visited[key] {
		return
	}
	visited[key] = true

	children := gc.graph.GetChildren(key)
	for _, child := range children {
		if child.ManagedBy == "" {
			gc.updateNodeAppName(child.Key, appName)
			gc.propagateAppNameRecursive(child.Key, appName, visited)
		}
	}
}

// ensureDescendantWatches ensures watches exist for descendant resource types.
// Descendant watches do NOT use label selectors since child resources typically
// lack Argo CD tracking labels.
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

		// Use descendant watch (no label selector) since child resources
		// typically don't have tracking labels
		created, err := gc.watchManager.EnsureWatchForDescendant(childGK)
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

// ensureWatchAndDescendants creates a watch for a resource type and its known descendants.
// Called when a cache miss triggers API fallback to auto-discover new resource types.
func (gc *GraphCache) ensureWatchAndDescendants(gk schema.GroupKind) {
	created, err := gc.watchManager.EnsureWatch(gk, "")
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"component": "graph-cache",
			"group":     gk.Group,
			"kind":      gk.Kind,
		}).Warn("Failed to create watch for discovered type")
		return
	}
	if created {
		gc.discoveryLock.Lock()
		gc.discoveredTypes[gk] = true
		gc.discoveryLock.Unlock()

		gc.metricsLock.Lock()
		gc.metrics.WatchesByType[gk] = true
		gc.metricsLock.Unlock()

		gc.ensureDescendantWatches(gk)
	}
}

// getTrackingMethod returns the current tracking method, safe for concurrent access.
func (gc *GraphCache) getTrackingMethod() graphcore.TrackingMethod {
	gc.configLock.RLock()
	defer gc.configLock.RUnlock()
	return gc.trackingMethod
}

// SetAppInstanceLabelKey updates the custom tracking label key used for pre-filtering.
func (gc *GraphCache) SetAppInstanceLabelKey(key string) {
	gc.configLock.Lock()
	gc.appInstanceLabelKey = key
	gc.configLock.Unlock()
}

// hasTrackingMarkers performs a fast check to determine if a resource might be
// managed by Argo CD. This is a pre-filter to avoid expensive operations
// (DeepCopy, health computation, graph storage) on unmanaged resources.
// Returns true if the resource has any tracking markers or OwnerReferences
// (potential child of a managed resource).
func (gc *GraphCache) hasTrackingMarkers(obj *unstructured.Unstructured) bool {
	// Check standard tracking label
	labels := obj.GetLabels()
	if _, ok := labels[graphcore.LabelKeyAppInstance]; ok {
		return true
	}

	// Check custom tracking label if configured
	gc.configLock.RLock()
	customKey := gc.appInstanceLabelKey
	gc.configLock.RUnlock()
	if customKey != "" && customKey != graphcore.LabelKeyAppInstance {
		if _, ok := labels[customKey]; ok {
			return true
		}
	}

	// Check tracking annotation
	annotations := obj.GetAnnotations()
	if _, ok := annotations[graphcore.AnnotationKeyAppInstance]; ok {
		return true
	}

	// Has OwnerReferences — could be a child of a managed resource
	if len(obj.GetOwnerReferences()) > 0 {
		return true
	}

	return false
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
		// Deep copy old node before update — addResourceToGraph mutates in-place
		key := graphcore.ToResourceKey(obj)
		var oldNode *graphcore.ResourceNode
		if existing, found := gc.graph.Get(key); found {
			copied := *existing
			oldNode = &copied
		}

		gc.addResourceToGraph(obj)

		// Get the new node after update
		newNode, exists := gc.graph.Get(key)
		if !exists && oldNode == nil {
			// Resource was skipped by pre-filter (unmanaged)
			gc.metricsLock.Lock()
			gc.metrics.SkippedEvents++
			gc.metricsLock.Unlock()
			return
		}

		gc.metricsLock.Lock()
		if eventType == watch.Added {
			gc.metrics.AddEvents++
		} else {
			gc.metrics.UpdateEvents++
		}
		gc.metricsLock.Unlock()

		// Skip callback if nothing meaningful changed (same health + same manifest hash).
		// This mirrors the traditional cache's ignoreResourceUpdates behavior and prevents
		// every resourceVersion bump from triggering full app reconciliation.
		if eventType == watch.Modified && oldNode != nil && newNode != nil {
			if gc.shouldSkipResourceUpdate(oldNode, newNode) {
				gc.metricsLock.Lock()
				gc.metrics.SkippedEvents++
				gc.metricsLock.Unlock()
				return
			}
		}

		// Notify adapter of resource change
		if cb := gc.getResourceUpdateCallback(); cb != nil {
			cb(newNode, oldNode, obj, eventType)
		}

		// Check if this is a new resource type
		gk := graphcore.ToGroupKind(obj)
		gc.discoveryLock.RLock()
		discovered := gc.discoveredTypes[gk]
		gc.discoveryLock.RUnlock()

		if !discovered {
			gc.discoveryLock.Lock()
			gc.discoveredTypes[gk] = true
			gc.discoveryLock.Unlock()

			// Ensure descendant watches
			gc.ensureDescendantWatches(gk)
		}

		// If a CRD was just created or updated, retry watches for types that previously failed
		if kube.IsCRD(obj) {
			gc.handleCRDEvent(obj)
		}

	case watch.Deleted:
		key := graphcore.ToResourceKey(obj)
		oldNode, _ := gc.graph.Get(key)
		gc.graph.Delete(key)

		gc.metricsLock.Lock()
		gc.metrics.DeleteEvents++
		gc.metricsLock.Unlock()

		// Notify adapter of deletion
		if gc.onResourceUpdated != nil {
			gc.onResourceUpdated(nil, oldNode, obj, eventType)
		}

		log.WithFields(log.Fields{
			"component": "graph-cache",
			"kind":      key.Kind,
			"namespace": key.Namespace,
			"name":      key.Name,
		}).Debug("Deleted resource from graph")
	}
}

// handleCRDEvent is called when a CRD add/modify event is detected.
// It invalidates the API resource cache and retries all pending watches that previously
// failed because their CRD didn't exist yet.
func (gc *GraphCache) handleCRDEvent(obj *unstructured.Unstructured) {
	crdName := obj.GetName()

	// Extract the group and kind that this CRD defines
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if spec == nil {
		return
	}
	group, _, _ := unstructured.NestedString(obj.Object, "spec", "group")
	kind, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "kind")

	log.WithFields(log.Fields{
		"component": "graph-cache",
		"crd":       crdName,
		"group":     group,
		"kind":      kind,
	}).Info("CRD event detected, retrying pending watches")

	// Invalidate the cached API resources so the watch manager re-discovers available types
	gc.watchManager.InvalidateAPIResourceCache()

	// Copy pending GVKs under lock so we can retry without holding the lock
	gc.discoveryLock.Lock()
	pending := make(map[schema.GroupVersionKind]string, len(gc.pendingWatchGVKs))
	for gvk, ns := range gc.pendingWatchGVKs {
		pending[gvk] = ns
	}
	gc.discoveryLock.Unlock()

	if len(pending) == 0 {
		return
	}

	for gvk, ns := range pending {
		gk := schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}
		created, err := gc.watchManager.EnsureWatch(gk, ns)
		if err != nil {
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"gvk":       gvk.String(),
			}).WithError(err).Debug("Pending watch still failing, will retry later")
			continue
		}
		if created {
			gc.discoveryLock.Lock()
			gc.discoveredTypes[gk] = true
			delete(gc.pendingWatchGVKs, gvk)
			gc.discoveryLock.Unlock()

			gc.metricsLock.Lock()
			gc.metrics.WatchesByType[gk] = true
			gc.metricsLock.Unlock()

			log.WithFields(log.Fields{
				"component": "graph-cache",
				"gvk":       gvk.String(),
				"namespace": ns,
			}).Info("Successfully created watch for previously pending type after CRD creation")
		}
	}
}

// periodicDiscovery runs discovery periodically to catch new resources.
func (gc *GraphCache) periodicDiscovery() {
	ticker := time.NewTicker(gc.graphConfig.DiscoveryInterval)
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

// periodicMetricsExport exports metrics to Prometheus periodically.
func (gc *GraphCache) periodicMetricsExport() {
	ticker := time.NewTicker(gc.graphConfig.MetricsExportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-gc.ctx.Done():
			return
		case <-ticker.C:
			gc.exportPrometheusMetrics()
		}
	}
}

// periodicRelationshipPersistence saves learned relationships to the store periodically.
func (gc *GraphCache) periodicRelationshipPersistence() {
	ticker := time.NewTicker(gc.graphConfig.PersistenceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-gc.ctx.Done():
			return
		case <-ticker.C:
			if err := gc.saveRelationships(); err != nil {
				log.WithError(err).WithField("component", "graph-cache").Warn("Failed to persist relationships")
			}
		}
	}
}

// saveRelationships persists all learned type relationships to the store.
func (gc *GraphCache) saveRelationships() error {
	allRels := gc.typeRelationships.GetAllLearnedRelationships()
	persisted := make([]graphcore.PersistedRelationship, 0, len(allRels))
	for _, rel := range allRels {
		persisted = append(persisted, graphcore.PersistedRelationship{
			Parent: graphcore.GvkToString(rel.Parent),
			Child:  graphcore.GvkToString(rel.Child),
		})
	}

	wellKnown := graphcore.NewTypeRelationshipCache()
	seeded := len(wellKnown.GetAllLearnedRelationships())
	learned := len(persisted) - seeded
	if learned < 0 {
		learned = 0
	}

	return gc.relationshipStore.Save(persisted, graphcore.PersistedRelationshipMetadata{
		TotalRelationships: len(persisted),
		SeededCount:        seeded,
		LearnedCount:       learned,
	})
}

// exportPrometheusMetrics exports current metrics to Prometheus.
func (gc *GraphCache) exportPrometheusMetrics() {
	if gc.prometheusMetrics == nil {
		return
	}

	metrics := gc.GetMetrics()
	server := gc.serverURL
	if server == "" {
		server = "default"
	}

	// Export basic metrics
	gc.prometheusMetrics.SetTotalResources(server, metrics.TotalManagedResources)
	gc.prometheusMetrics.SetWatchedTypes(server, len(metrics.WatchesByType))
	gc.prometheusMetrics.SetApplications(server, metrics.UniqueApplications)

	// Estimate graph cache memory from node count.
	// Each ResourceNode is roughly 2KB (key, metadata, labels, annotations, refs)
	// plus ~4KB when the full unstructured object is stored.
	const estimatedBytesPerNode = 6 * 1024
	gc.prometheusMetrics.SetMemoryBytes(server, int64(metrics.TotalManagedResources)*estimatedBytesPerNode)

	// Export relationship metrics
	if gc.typeRelationships != nil {
		allRels := gc.typeRelationships.GetAllLearnedRelationships()
		gc.prometheusMetrics.SetRelationshipsLearned("all", len(allRels))
	}
}

// GetDynamicClient returns the dynamic client used by the cache
func (gc *GraphCache) GetDynamicClient() dynamic.Interface {
	return gc.watchManager.GetDynamicClient()
}

// GetMetrics returns current cache metrics.
func (gc *GraphCache) GetMetrics() CacheMetrics {
	// Gather external metrics outside the lock to avoid holding lock over I/O
	graphMetrics := gc.graph.GetMetrics()
	watchMetrics := gc.watchManager.GetMetrics()

	gc.metricsLock.Lock()
	defer gc.metricsLock.Unlock()

	// Combine metrics
	gc.metrics.TotalManagedResources = graphMetrics.TotalResources
	gc.metrics.ResourcesByType = graphMetrics.ResourcesByType
	gc.metrics.ResourcesByApplication = graphMetrics.ResourcesByApp
	gc.metrics.UniqueApplications = graphMetrics.UniqueApplications
	gc.metrics.UniqueResourceTypes = graphMetrics.UniqueResourceTypes
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

// HealthStatus represents the health state of the graph cache.
type HealthStatus struct {
	Healthy            bool          `json:"healthy"`
	TotalResources     int           `json:"totalResources"`
	ActiveWatches      int           `json:"activeWatches"`
	FailedWatches      []string      `json:"failedWatches,omitempty"`
	LastDiscoveryTime  time.Time     `json:"lastDiscoveryTime"`
	LastDiscoveryError string        `json:"lastDiscoveryError,omitempty"`
	MemoryUsageMB      int64         `json:"memoryUsageMb"`
	Alerts             []HealthAlert `json:"alerts,omitempty"`
}

// HealthAlert represents a health check alert.
type HealthAlert struct {
	Severity string `json:"severity"` // warning, critical
	Message  string `json:"message"`
}

// HealthCheck performs a comprehensive health check of the graph cache.
// Returns a HealthStatus with any alerts and overall health state.
func (gc *GraphCache) HealthCheck() HealthStatus {
	metrics := gc.GetMetrics()

	status := HealthStatus{
		Healthy:           true,
		TotalResources:    metrics.TotalManagedResources,
		ActiveWatches:     metrics.ActiveWatches,
		LastDiscoveryTime: metrics.LastDiscoveryTime,
	}

	// Check 1: No resources after initial discovery
	if metrics.DiscoveryRuns > 0 && metrics.TotalManagedResources == 0 {
		status.Alerts = append(status.Alerts, HealthAlert{
			Severity: "warning",
			Message:  "No managed resources found after discovery",
		})
	}

	// Check 2: No active watches
	if metrics.ActiveWatches == 0 {
		status.Alerts = append(status.Alerts, HealthAlert{
			Severity: "critical",
			Message:  "No active watches established",
		})
		status.Healthy = false
	}

	// Check 3: Stale discovery (only check if we've done at least one discovery)
	if !metrics.LastDiscoveryTime.IsZero() && time.Since(metrics.LastDiscoveryTime) > 10*time.Minute {
		status.Alerts = append(status.Alerts, HealthAlert{
			Severity: "warning",
			Message:  fmt.Sprintf("Discovery stale (last run: %v ago)", time.Since(metrics.LastDiscoveryTime)),
		})
	}

	// Check 4: High estimated memory usage (>2GB)
	const estimatedBytesPerNode = 6 * 1024
	status.MemoryUsageMB = int64(metrics.TotalManagedResources) * estimatedBytesPerNode / (1024 * 1024)

	if status.MemoryUsageMB > 2048 {
		status.Alerts = append(status.Alerts, HealthAlert{
			Severity: "warning",
			Message:  fmt.Sprintf("High estimated memory usage: %dMB", status.MemoryUsageMB),
		})
	}

	// Check 5: Failed watches (if watch manager provides this info)
	watchMetrics := gc.watchManager.GetMetrics()
	if watchMetrics.LastDiscoveryTime.IsZero() {
		// Watch manager hasn't completed discovery yet
		status.Alerts = append(status.Alerts, HealthAlert{
			Severity: "warning",
			Message:  "Watch manager discovery not yet completed",
		})
	}

	return status
}

// shouldSkipResourceUpdate returns true if a resource update can be safely ignored
// because neither health status nor manifest content changed. This prevents every
// resourceVersion bump (e.g. status-only updates) from triggering app reconciliation.
func (gc *GraphCache) shouldSkipResourceUpdate(oldNode, newNode *graphcore.ResourceNode) bool {
	if oldNode.CachedInfo == nil || newNode.CachedInfo == nil {
		return false
	}
	oldInfo, oldOk := oldNode.CachedInfo.(*statecache.ResourceInfo)
	newInfo, newOk := newNode.CachedInfo.(*statecache.ResourceInfo)
	if !oldOk || !newOk {
		return false
	}
	return statecache.SkipResourceUpdate(oldInfo, newInfo)
}

// GetResourcesByApplication returns all resources managed by an application.
func (gc *GraphCache) GetResourcesByApplication(appName string) []*graphcore.ResourceNode {
	return gc.graph.GetByApplication(appName)
}

// GetByType returns all resources of a specific GroupKind.
func (gc *GraphCache) GetByType(gk schema.GroupKind) []*graphcore.ResourceNode {
	return gc.graph.GetByType(gk)
}

// GetByLabel returns all resources matching a label key and value.
func (gc *GraphCache) GetByLabel(key, value string) []*graphcore.ResourceNode {
	return gc.graph.GetByLabel(key, value)
}

// GetAllTypes returns all resource types in the cache.
func (gc *GraphCache) GetAllTypes() []schema.GroupKind {
	return gc.graph.GetAllTypes()
}

// GetAllNodes returns all nodes in the cache.
func (gc *GraphCache) GetAllNodes() []*graphcore.ResourceNode {
	return gc.graph.GetAllNodes()
}

// GetResource retrieves a specific resource from the cache.
func (gc *GraphCache) GetResource(key kube.ResourceKey) (*graphcore.ResourceNode, bool) {
	return gc.graph.Get(key)
}

// GetChildren returns all children of a resource.
func (gc *GraphCache) GetChildren(key kube.ResourceKey) []*graphcore.ResourceNode {
	return gc.graph.GetChildren(key)
}

// GetParents returns all parents of a resource.
func (gc *GraphCache) GetParents(key kube.ResourceKey) []*graphcore.ResourceNode {
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

	log.WithFields(log.Fields{
		"component":     "graph-cache",
		"app_name":      app.Name,
		"app_namespace": app.Namespace,
	}).Info("Preparing watches for application")

	err := gc.manifestDiscovery.DiscoverFromApplication(ctx, app)
	if err != nil {
		log.WithError(err).Error("Failed to prepare watches for application")
		return err
	}
	log.WithField("component", "graph-cache").Info("Watches prepared for application")
	return nil
}

// LearnResourceRelationships learns parent-child relationships from actual resources
// This is called when resources are observed in the cluster to build the type relationship cache
func (gc *GraphCache) LearnResourceRelationships(parent, child *unstructured.Unstructured) {
	parentGVK := parent.GroupVersionKind()
	childGVK := child.GroupVersionKind()

	gc.typeRelationships.LearnRelationship(parentGVK, childGVK)

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
	created, err := gc.watchManager.EnsureWatch(gk, namespace)
	if err != nil {
		gc.discoveryLock.Lock()
		gc.pendingWatchGVKs[gvk] = namespace
		gc.discoveryLock.Unlock()
		return fmt.Errorf("failed to create watch for %s: %w", gvk.String(), err)
	}

	if created {
		gc.discoveryLock.Lock()
		gc.discoveredTypes[gk] = true
		delete(gc.pendingWatchGVKs, gvk)
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

// SetResourceUpdateCallback registers a callback invoked on every resource add/update/delete.
// Must be called before Start() to avoid races with watch goroutines.
func (gc *GraphCache) SetResourceUpdateCallback(cb func(newRes, oldRes *graphcore.ResourceNode, obj *unstructured.Unstructured, eventType watch.EventType)) {
	gc.configLock.Lock()
	gc.onResourceUpdated = cb
	gc.configLock.Unlock()
}

// getResourceUpdateCallback returns the current callback, safe for concurrent access.
func (gc *GraphCache) getResourceUpdateCallback() func(newRes, oldRes *graphcore.ResourceNode, obj *unstructured.Unstructured, eventType watch.EventType) {
	gc.configLock.RLock()
	defer gc.configLock.RUnlock()
	return gc.onResourceUpdated
}

// GetTypeRelationships returns the type relationship cache for inspection
func (gc *GraphCache) GetTypeRelationships() *graphcore.TypeRelationshipCache {
	return gc.typeRelationships
}
