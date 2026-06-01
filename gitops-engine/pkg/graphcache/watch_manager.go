package graphcache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
)

// SelectiveWatchManager manages dynamic watches for resource types that are actually managed by Argo CD.
type SelectiveWatchManager struct {
	// Kubernetes clients
	dynamicClient   dynamic.Interface
	discoveryClient discovery.DiscoveryInterface

	// Active watches
	watches   map[schema.GroupKind]*WatchHandle
	watchLock sync.RWMutex

	// Tracking configuration
	trackingMethod TrackingMethod

	// Watched namespaces (empty means all namespaces)
	namespaces []string

	// Callbacks
	onResourceEvent ResourceEventHandler

	// Configuration
	graphConfig GraphConfig

	// Logger
	log logr.Logger

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// Cached API discovery results
	discoveryLock          sync.RWMutex
	cachedAPIResources     []*metav1.APIResourceList
	cachedAPIResourcesTime time.Time

	// Metrics
	metricsLock sync.RWMutex
	metrics     WatchMetrics
}

// WatchHandle represents an active watch for a specific resource type.
type WatchHandle struct {
	GroupKind      schema.GroupKind
	Namespaces     []string // Initial configured namespaces
	Cancel         context.CancelFunc
	ResourceCount  int // Number of resources watched
	StartTime      time.Time
	lastEventNanos atomic.Int64 // Unix nano timestamp of last event, use SetLastEventTime/GetLastEventTime
	IsNamespaced   bool

	// Dynamic namespace tracking
	watchedNamespaces map[string]bool
	lock              sync.Mutex

	// Store GVR and Context for extension
	gvr schema.GroupVersionResource
	ctx context.Context
}

// SetLastEventTime records the time of the last event.
func (h *WatchHandle) SetLastEventTime(t time.Time) {
	h.lastEventNanos.Store(t.UnixNano())
}

// GetLastEventTime returns the time of the last event.
func (h *WatchHandle) GetLastEventTime() time.Time {
	nanos := h.lastEventNanos.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// ResourceEventHandler is called when a resource event occurs.
type ResourceEventHandler func(eventType watch.EventType, obj *unstructured.Unstructured)

// WatchMetrics contains metrics about the watch manager.
type WatchMetrics struct {
	ActiveWatches     int
	TotalEvents       int64
	WatchesByType     map[schema.GroupKind]int
	EventsByType      map[watch.EventType]int64
	LastDiscoveryTime time.Time
	DiscoveryDuration time.Duration
}

// NewSelectiveWatchManager creates a new selective watch manager.
// The provided parent context controls the lifecycle of all watches.
func NewSelectiveWatchManager(
	parentCtx context.Context,
	dynamicClient dynamic.Interface,
	discoveryClient discovery.DiscoveryInterface,
	trackingMethod TrackingMethod,
	namespaces []string,
	handler ResourceEventHandler,
	graphConfig GraphConfig,
	log logr.Logger,
) *SelectiveWatchManager {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(parentCtx)

	return &SelectiveWatchManager{
		dynamicClient:   dynamicClient,
		discoveryClient: discoveryClient,
		trackingMethod:  trackingMethod,
		namespaces:      namespaces,
		onResourceEvent: handler,
		graphConfig:     graphConfig,
		log:             log,
		watches:         make(map[schema.GroupKind]*WatchHandle),
		ctx:             ctx,
		cancel:          cancel,
		metrics: WatchMetrics{
			WatchesByType: make(map[schema.GroupKind]int),
			EventsByType:  make(map[watch.EventType]int64),
		},
	}
}

// EnsureWatch ensures a watch exists for the given resource type.
// If a watch already exists, this is a no-op unless namespace is provided and not yet watched.
// resourceVersion, if non-empty, is used as the starting point for the watch to avoid
// missing events between a prior List and this Watch call.
// Returns true if a new watch (or new namespace watch) was created.
func (wm *SelectiveWatchManager) EnsureWatch(gk schema.GroupKind, namespace string, resourceVersion ...string) (bool, error) {
	wm.watchLock.Lock()
	defer wm.watchLock.Unlock()

	// Check if watch already exists
	if handle, exists := wm.watches[gk]; exists {
		// If namespace is provided, ensure it is watched
		if namespace != "" && handle.IsNamespaced {
			return wm.extendWatch(handle, namespace)
		}
		return false, nil
	}

	// Discover resource information
	gvr, isNamespaced, err := wm.DiscoverResource(gk)
	if err != nil {
		return false, fmt.Errorf("failed to discover resource %s: %w", gk, err)
	}

	rv := ""
	if len(resourceVersion) > 0 {
		rv = resourceVersion[0]
	}

	// Create watch
	handle, err := wm.createWatch(gk, gvr, isNamespaced, namespace, rv)
	if err != nil {
		return false, fmt.Errorf("failed to create watch for %s: %w", gk, err)
	}

	wm.watches[gk] = handle

	// Update metrics
	wm.metricsLock.Lock()
	wm.metrics.ActiveWatches++
	wm.metrics.WatchesByType[gk]++
	wm.metricsLock.Unlock()

	wm.log.Info("Created new watch for resource type",
		"group", gk.Group, "kind", gk.Kind, "namespaced", isNamespaced,
		"namespaces", wm.namespaces, "extra_ns", namespace)

	return true, nil
}

// RemoveWatch removes a watch for the given resource type.
func (wm *SelectiveWatchManager) RemoveWatch(gk schema.GroupKind) {
	wm.watchLock.Lock()
	defer wm.watchLock.Unlock()

	handle, exists := wm.watches[gk]
	if !exists {
		return
	}

	// Stop the watch
	handle.Cancel()

	delete(wm.watches, gk)

	// Update metrics
	wm.metricsLock.Lock()
	wm.metrics.ActiveWatches--
	delete(wm.metrics.WatchesByType, gk)
	wm.metricsLock.Unlock()

	wm.log.Info("Removed watch for resource type", "group", gk.Group, "kind", gk.Kind)
}

// removeDeadWatch removes a watch that has exceeded max consecutive failures.
// This ensures IsTypeWatched returns false so future EnsureWatch calls can retry.
func (wm *SelectiveWatchManager) removeDeadWatch(gk schema.GroupKind) {
	wm.watchLock.Lock()
	defer wm.watchLock.Unlock()

	handle, exists := wm.watches[gk]
	if !exists {
		return
	}

	handle.Cancel()
	delete(wm.watches, gk)

	wm.metricsLock.Lock()
	wm.metrics.ActiveWatches--
	delete(wm.metrics.WatchesByType, gk)
	wm.metricsLock.Unlock()

	wm.log.Info("Watch removed after exceeding max consecutive failures", "group", gk.Group, "kind", gk.Kind)
}

// GetActiveWatches returns the list of currently active watches.
func (wm *SelectiveWatchManager) GetActiveWatches() []schema.GroupKind {
	wm.watchLock.RLock()
	defer wm.watchLock.RUnlock()

	watches := make([]schema.GroupKind, 0, len(wm.watches))
	for gk := range wm.watches {
		watches = append(watches, gk)
	}

	return watches
}

// GetMetrics returns current watch metrics.
func (wm *SelectiveWatchManager) GetMetrics() WatchMetrics {
	wm.metricsLock.RLock()
	defer wm.metricsLock.RUnlock()

	// Deep copy to avoid race conditions
	metrics := WatchMetrics{
		ActiveWatches:     wm.metrics.ActiveWatches,
		TotalEvents:       wm.metrics.TotalEvents,
		WatchesByType:     make(map[schema.GroupKind]int),
		EventsByType:      make(map[watch.EventType]int64),
		LastDiscoveryTime: wm.metrics.LastDiscoveryTime,
		DiscoveryDuration: wm.metrics.DiscoveryDuration,
	}

	for k, v := range wm.metrics.WatchesByType {
		metrics.WatchesByType[k] = v
	}

	for k, v := range wm.metrics.EventsByType {
		metrics.EventsByType[k] = v
	}

	return metrics
}

// GetDiscoveryClient returns the discovery client used by the watch manager.
func (wm *SelectiveWatchManager) GetDiscoveryClient() discovery.DiscoveryInterface {
	return wm.discoveryClient
}

// GetDynamicClient returns the dynamic client used by the watch manager.
func (wm *SelectiveWatchManager) GetDynamicClient() dynamic.Interface {
	return wm.dynamicClient
}

// Shutdown stops all watches and cleans up resources.
func (wm *SelectiveWatchManager) Shutdown() {
	wm.cancel()

	wm.watchLock.Lock()
	defer wm.watchLock.Unlock()

	for gk, handle := range wm.watches {
		handle.Cancel()
		delete(wm.watches, gk)
	}

	wm.log.Info("Selective watch manager shutdown complete")
}

// extendWatch ensures that the given namespace is being watched for the handle
func (wm *SelectiveWatchManager) extendWatch(handle *WatchHandle, namespace string) (bool, error) {
	handle.lock.Lock()
	defer handle.lock.Unlock()

	// If we are watching all namespaces
	if len(wm.namespaces) == 0 {
		return false, nil // Already watching all
	}

	if handle.watchedNamespaces[namespace] {
		return false, nil // Already watching this namespace
	}

	listOpts := metav1.ListOptions{}

	go wm.startWatcher(handle.ctx, handle.gvr, namespace, listOpts, handle)

	handle.watchedNamespaces[namespace] = true
	wm.log.Info("Extended watch to include namespace", "namespace", namespace)

	return true, nil
}

// createWatch creates a watch for the specified resource type.
// If resourceVersion is non-empty, the watch starts from that version for List-Watch consistency.
func (wm *SelectiveWatchManager) createWatch(gk schema.GroupKind, gvr schema.GroupVersionResource, isNamespaced bool, extraNamespace string, resourceVersion string) (*WatchHandle, error) {
	ctx, cancel := context.WithCancel(wm.ctx)

	handle := &WatchHandle{
		GroupKind:         gk,
		Namespaces:        wm.namespaces,
		Cancel:            cancel,
		StartTime:         time.Now(),
		IsNamespaced:      isNamespaced,
		watchedNamespaces: make(map[string]bool),
		gvr:               gvr,
		ctx:               ctx,
	}

	listOpts := metav1.ListOptions{}
	if resourceVersion != "" {
		listOpts.ResourceVersion = resourceVersion
	}

	// Start watches based on scope
	if isNamespaced {
		if len(wm.namespaces) == 0 {
			// Watch all namespaces
			go wm.startWatcher(ctx, gvr, "", listOpts, handle)
		} else {
			// Watch specific namespaces
			namespacesToWatch := make(map[string]bool)
			for _, ns := range wm.namespaces {
				namespacesToWatch[ns] = true
			}
			if extraNamespace != "" {
				namespacesToWatch[extraNamespace] = true
			}

			for ns := range namespacesToWatch {
				go wm.startWatcher(ctx, gvr, ns, listOpts, handle)
				handle.watchedNamespaces[ns] = true
			}
		}
	} else {
		// Cluster-scoped resource
		go wm.startWatcher(ctx, gvr, "", listOpts, handle)
	}

	return handle, nil
}

// startWatcher establishes a watch and handles auto-recovery with backoff and max retry protection
func (wm *SelectiveWatchManager) startWatcher(ctx context.Context, gvr schema.GroupVersionResource, namespace string, listOpts metav1.ListOptions, handle *WatchHandle) {
	defer func() {
		if r := recover(); r != nil {
			wm.log.Error(fmt.Errorf("panic: %v", r), "Panic in watch event processor",
				"group", handle.GroupKind.Group, "kind", handle.GroupKind.Kind)
		}
	}()

	maxConsecutiveFailures := wm.graphConfig.MaxConsecutiveFailures
	consecutiveFailures := 0

	minRetry := wm.graphConfig.MinRetryInterval
	maxRetry := wm.graphConfig.MaxRetryInterval
	retryInterval := minRetry

	for {
		// Check for cancellation
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Goroutine leak protection: stop after too many consecutive failures
		if consecutiveFailures >= maxConsecutiveFailures {
			wm.log.Error(nil, "Max consecutive watch failures exceeded, stopping watcher to prevent goroutine leak",
				"group", handle.GroupKind.Group, "kind", handle.GroupKind.Kind,
				"namespace", namespace, "consecutive_fails", consecutiveFailures)
			wm.removeDeadWatch(handle.GroupKind)
			return
		}

		// Establish watch
		var watcher watch.Interface
		var err error

		if namespace != "" {
			watcher, err = wm.dynamicClient.Resource(gvr).Namespace(namespace).Watch(ctx, listOpts)
		} else {
			watcher, err = wm.dynamicClient.Resource(gvr).Watch(ctx, listOpts)
		}

		if err != nil {
			consecutiveFailures++

			wm.log.Info("Failed to watch resource, retrying",
				"group", handle.GroupKind.Group, "kind", handle.GroupKind.Kind,
				"error", err, "consecutive_fails", consecutiveFailures, "retry_in", retryInterval)

			// Backoff
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryInterval):
				retryInterval *= 2
				if retryInterval > maxRetry {
					retryInterval = maxRetry
				}
				continue
			}
		}

		// Watch established successfully - reset failure counter
		consecutiveFailures = 0
		retryInterval = minRetry

		// Process events
		func() {
			defer watcher.Stop()
			resultChan := watcher.ResultChan()

			for {
				select {
				case <-ctx.Done():
					return
				case event, ok := <-resultChan:
					if !ok {
						// Channel closed
						wm.log.Info("Watch closed unexpectedly, reconnecting",
							"group", handle.GroupKind.Group, "kind", handle.GroupKind.Kind)
						return
					}

					// Update metrics
					handle.SetLastEventTime(time.Now())
					wm.metricsLock.Lock()
					wm.metrics.TotalEvents++
					wm.metrics.EventsByType[event.Type]++
					wm.metricsLock.Unlock()

					// Process event
					if obj, ok := event.Object.(*unstructured.Unstructured); ok {
						wm.handleEvent(event.Type, obj)
					}
				}
			}
		}()
	}
}

// handleEvent handles a single watch event.
// All resources from watched types are processed, not just those with Argo CD tracking.
// Child resources (ReplicaSets, Pods) typically lack tracking labels but are linked
// to applications via OwnerReferences. The downstream addResourceToGraph handles
// deriving app ownership from the parent chain.
func (wm *SelectiveWatchManager) handleEvent(eventType watch.EventType, obj *unstructured.Unstructured) {
	if wm.onResourceEvent != nil {
		wm.onResourceEvent(eventType, obj)
	}
}

// DiscoverResource discovers the GVR and scope for a GroupKind.
func (wm *SelectiveWatchManager) DiscoverResource(gk schema.GroupKind) (schema.GroupVersionResource, bool, error) {
	startTime := time.Now()
	defer func() {
		wm.metricsLock.Lock()
		wm.metrics.LastDiscoveryTime = time.Now()
		wm.metrics.DiscoveryDuration = time.Since(startTime)
		wm.metricsLock.Unlock()
	}()

	apiResourceLists := wm.getCachedAPIResources()

	// Find matching resource
	for _, apiResourceList := range apiResourceLists {
		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			continue
		}

		// Check if group matches
		if gv.Group != gk.Group {
			continue
		}

		for _, apiResource := range apiResourceList.APIResources {
			if apiResource.Kind == gk.Kind {
				gvr := schema.GroupVersionResource{
					Group:    gv.Group,
					Version:  gv.Version,
					Resource: apiResource.Name,
				}
				return gvr, apiResource.Namespaced, nil
			}
		}
	}

	return schema.GroupVersionResource{}, false, fmt.Errorf("resource %s not found", gk)
}

const apiResourceCacheTTL = 2 * time.Minute

func (wm *SelectiveWatchManager) getCachedAPIResources() []*metav1.APIResourceList {
	wm.discoveryLock.RLock()
	if wm.cachedAPIResources != nil && time.Since(wm.cachedAPIResourcesTime) < apiResourceCacheTTL {
		result := wm.cachedAPIResources
		wm.discoveryLock.RUnlock()
		return result
	}
	wm.discoveryLock.RUnlock()

	wm.discoveryLock.Lock()
	defer wm.discoveryLock.Unlock()

	// Double-check after acquiring write lock
	if wm.cachedAPIResources != nil && time.Since(wm.cachedAPIResourcesTime) < apiResourceCacheTTL {
		return wm.cachedAPIResources
	}

	apiResourceLists, err := wm.discoveryClient.ServerPreferredResources()
	if err != nil {
		wm.log.V(1).Info("Partial error during API discovery", "error", err)
	}
	wm.cachedAPIResources = apiResourceLists
	wm.cachedAPIResourcesTime = time.Now()
	return apiResourceLists
}

// InvalidateAPIResourceCache forces the next discoverResource call to re-fetch API resources.
func (wm *SelectiveWatchManager) InvalidateAPIResourceCache() {
	wm.discoveryLock.Lock()
	defer wm.discoveryLock.Unlock()
	wm.cachedAPIResources = nil
}

// ListManagedResources performs an initial list of all resources with Argo CD tracking.
// Returns the resources and the latest ResourceVersion for establishing a consistent watch.
func (wm *SelectiveWatchManager) ListManagedResources(gvr schema.GroupVersionResource, isNamespaced bool) ([]*unstructured.Unstructured, string, error) {
	listOpts := metav1.ListOptions{}

	var result []*unstructured.Unstructured
	var latestRV string

	if isNamespaced {
		if len(wm.namespaces) == 0 {
			list, err := wm.dynamicClient.Resource(gvr).List(wm.ctx, listOpts)
			if err != nil {
				return nil, "", err
			}
			latestRV = list.GetResourceVersion()
			for i := range list.Items {
				result = append(result, &list.Items[i])
			}
		} else {
			for _, ns := range wm.namespaces {
				list, err := wm.dynamicClient.Resource(gvr).Namespace(ns).List(wm.ctx, listOpts)
				if err != nil {
					wm.log.Info("Failed to list resources in namespace", "namespace", ns, "error", err)
					continue
				}
				if list.GetResourceVersion() > latestRV {
					latestRV = list.GetResourceVersion()
				}
				for i := range list.Items {
					result = append(result, &list.Items[i])
				}
			}
		}
	} else {
		list, err := wm.dynamicClient.Resource(gvr).List(wm.ctx, listOpts)
		if err != nil {
			return nil, "", err
		}
		latestRV = list.GetResourceVersion()
		for i := range list.Items {
			result = append(result, &list.Items[i])
		}
	}

	return result, latestRV, nil
}

// ListAllResources lists all resources of a type without label selectors.
// Used for descendant resource types (ReplicaSets, Pods) that typically don't
// have Argo CD tracking labels but are linked via OwnerReferences.
func (wm *SelectiveWatchManager) ListAllResources(gvr schema.GroupVersionResource, isNamespaced bool) ([]*unstructured.Unstructured, error) {
	listOpts := metav1.ListOptions{}
	var result []*unstructured.Unstructured

	if isNamespaced {
		if len(wm.namespaces) == 0 {
			list, err := wm.dynamicClient.Resource(gvr).List(wm.ctx, listOpts)
			if err != nil {
				return nil, err
			}
			for i := range list.Items {
				result = append(result, &list.Items[i])
			}
		} else {
			for _, ns := range wm.namespaces {
				list, err := wm.dynamicClient.Resource(gvr).Namespace(ns).List(wm.ctx, listOpts)
				if err != nil {
					wm.log.Info("Failed to list resources in namespace", "namespace", ns, "error", err)
					continue
				}
				for i := range list.Items {
					result = append(result, &list.Items[i])
				}
			}
		}
	} else {
		list, err := wm.dynamicClient.Resource(gvr).List(wm.ctx, listOpts)
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			result = append(result, &list.Items[i])
		}
	}

	return result, nil
}

// EnsureWatchForDescendant creates a watch without label selectors for descendant types.
// Descendant resources (e.g., ReplicaSets, Pods) typically don't have tracking labels.
func (wm *SelectiveWatchManager) EnsureWatchForDescendant(gk schema.GroupKind) (bool, error) {
	wm.watchLock.Lock()
	defer wm.watchLock.Unlock()

	if _, exists := wm.watches[gk]; exists {
		return false, nil
	}

	gvr, isNamespaced, err := wm.DiscoverResource(gk)
	if err != nil {
		return false, fmt.Errorf("failed to discover resource %s: %w", gk, err)
	}

	handle, err := wm.createWatchWithoutLabelSelector(gk, gvr, isNamespaced)
	if err != nil {
		return false, fmt.Errorf("failed to create descendant watch for %s: %w", gk, err)
	}

	wm.watches[gk] = handle

	wm.metricsLock.Lock()
	wm.metrics.ActiveWatches++
	wm.metrics.WatchesByType[gk]++
	wm.metricsLock.Unlock()

	wm.log.Info("Created descendant watch (no label selector)",
		"group", gk.Group, "kind", gk.Kind)

	return true, nil
}

// createWatchWithoutLabelSelector creates a watch without label selectors.
// Used for descendant types that lack tracking labels.
func (wm *SelectiveWatchManager) createWatchWithoutLabelSelector(gk schema.GroupKind, gvr schema.GroupVersionResource, isNamespaced bool) (*WatchHandle, error) {
	ctx, cancel := context.WithCancel(wm.ctx)

	handle := &WatchHandle{
		GroupKind:         gk,
		Namespaces:        wm.namespaces,
		Cancel:            cancel,
		StartTime:         time.Now(),
		IsNamespaced:      isNamespaced,
		watchedNamespaces: make(map[string]bool),
		gvr:               gvr,
		ctx:               ctx,
	}

	listOpts := metav1.ListOptions{}

	if isNamespaced {
		if len(wm.namespaces) == 0 {
			go wm.startWatcher(ctx, gvr, "", listOpts, handle)
		} else {
			for _, ns := range wm.namespaces {
				go wm.startWatcher(ctx, gvr, ns, listOpts, handle)
				handle.watchedNamespaces[ns] = true
			}
		}
	} else {
		go wm.startWatcher(ctx, gvr, "", listOpts, handle)
	}

	return handle, nil
}

// StopAllWatches stops all active watches without shutting down the manager.
// Used during Invalidate to reset watch state.
func (wm *SelectiveWatchManager) StopAllWatches() {
	wm.watchLock.Lock()
	defer wm.watchLock.Unlock()

	for gk, handle := range wm.watches {
		handle.Cancel()
		delete(wm.watches, gk)
	}

	wm.metricsLock.Lock()
	wm.metrics.ActiveWatches = 0
	wm.metrics.WatchesByType = make(map[schema.GroupKind]int)
	wm.metricsLock.Unlock()
}

// IsTypeWatched returns whether a GroupKind is currently being watched.
func (wm *SelectiveWatchManager) IsTypeWatched(gk schema.GroupKind) bool {
	wm.watchLock.RLock()
	defer wm.watchLock.RUnlock()
	_, exists := wm.watches[gk]
	return exists
}

// GetWatchIsNamespaced returns whether a watched GroupKind is namespaced.
// Returns (isNamespaced, found). If the type is not watched, found is false.
func (wm *SelectiveWatchManager) GetWatchIsNamespaced(gk schema.GroupKind) (bool, bool) {
	wm.watchLock.RLock()
	defer wm.watchLock.RUnlock()
	handle, exists := wm.watches[gk]
	if !exists {
		return false, false
	}
	return handle.IsNamespaced, true
}

// ToResourceKey converts an unstructured object to a ResourceKey.
func ToResourceKey(obj *unstructured.Unstructured) kube.ResourceKey {
	gvk := obj.GroupVersionKind()
	return kube.ResourceKey{
		Group:     gvk.Group,
		Kind:      gvk.Kind,
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

// ToGroupKind converts an unstructured object to a GroupKind.
func ToGroupKind(obj *unstructured.Unstructured) schema.GroupKind {
	gvk := obj.GroupVersionKind()
	return schema.GroupKind{
		Group: gvk.Group,
		Kind:  gvk.Kind,
	}
}
