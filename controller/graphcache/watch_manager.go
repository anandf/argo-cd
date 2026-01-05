package graphcache

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
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

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// Metrics
	metricsLock sync.RWMutex
	metrics     WatchMetrics
}

// WatchHandle represents an active watch for a specific resource type.
type WatchHandle struct {
	GroupKind     schema.GroupKind
	Namespaces    []string // Initial configured namespaces
	Cancel        context.CancelFunc
	ResourceCount int // Number of resources watched
	StartTime     time.Time
	LastEventTime time.Time
	IsNamespaced  bool

	// Dynamic namespace tracking
	watchedNamespaces map[string]bool
	lock              sync.Mutex

	// Store GVR and Context for extension
	gvr schema.GroupVersionResource
	ctx context.Context
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
func NewSelectiveWatchManager(
	dynamicClient dynamic.Interface,
	discoveryClient discovery.DiscoveryInterface,
	trackingMethod TrackingMethod,
	namespaces []string,
	handler ResourceEventHandler,
) *SelectiveWatchManager {
	ctx, cancel := context.WithCancel(context.Background())

	return &SelectiveWatchManager{
		dynamicClient:   dynamicClient,
		discoveryClient: discoveryClient,
		trackingMethod:  trackingMethod,
		namespaces:      namespaces,
		onResourceEvent: handler,
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
// Returns true if a new watch (or new namespace watch) was created.
func (wm *SelectiveWatchManager) EnsureWatch(gk schema.GroupKind, namespace string) (bool, error) {
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
	gvr, isNamespaced, err := wm.discoverResource(gk)
	if err != nil {
		return false, fmt.Errorf("failed to discover resource %s: %w", gk, err)
	}

	// Create watch
	handle, err := wm.createWatch(gk, gvr, isNamespaced, namespace)
	if err != nil {
		return false, fmt.Errorf("failed to create watch for %s: %w", gk, err)
	}

	wm.watches[gk] = handle

	// Update metrics
	wm.metricsLock.Lock()
	wm.metrics.ActiveWatches++
	wm.metrics.WatchesByType[gk]++
	wm.metricsLock.Unlock()

	log.WithFields(log.Fields{
		"component":  "graph-cache",
		"group":      gk.Group,
		"kind":      gk.Kind,
		"namespaced": isNamespaced,
		"namespaces": wm.namespaces,
		"extra_ns":   namespace,
	}).Info("Created new watch for resource type")

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

	log.WithFields(log.Fields{
		"component": "graph-cache",
		"group":     gk.Group,
		"kind":      gk.Kind,
	}).Info("Removed watch for resource type")
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

	log.WithField("component", "graph-cache").Info("Selective watch manager shutdown complete")
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

	// Prepare list opts
	listOpts := metav1.ListOptions{
		Watch: true,
	}
	if wm.trackingMethod == TrackingMethodLabel || wm.trackingMethod == TrackingMethodAnnotationAndLabel {
		listOpts.LabelSelector = "app.kubernetes.io/instance"
	}

	// Start watcher using handle's context (so it gets cancelled with handle)
	go wm.startWatcher(handle.ctx, handle.gvr, namespace, listOpts, handle)
	
	handle.watchedNamespaces[namespace] = true
	log.WithField("namespace", namespace).Info("Extended watch to include namespace")
	
	return true, nil
}

// createWatch creates a watch for the specified resource type.
func (wm *SelectiveWatchManager) createWatch(gk schema.GroupKind, gvr schema.GroupVersionResource, isNamespaced bool, extraNamespace string) (*WatchHandle, error) {
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

	// Build list options with label selector for tracking
	listOpts := metav1.ListOptions{
		Watch: true,
	}

	// For label-based tracking, we can use a label selector
	if wm.trackingMethod == TrackingMethodLabel || wm.trackingMethod == TrackingMethodAnnotationAndLabel {
		// Watch resources with the app.kubernetes.io/instance label
		listOpts.LabelSelector = "app.kubernetes.io/instance"
	} else if wm.trackingMethod == TrackingMethodAnnotation {
		// Warning for inefficient annotation tracking
		log.WithFields(log.Fields{
			"component": "graph-cache",
			"kind":      gk.Kind,
		}).Warn("Watching resource with annotation tracking (inefficient). Consider using label tracking.")
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

// startWatcher establishes a watch and handles auto-recovery
func (wm *SelectiveWatchManager) startWatcher(ctx context.Context, gvr schema.GroupVersionResource, namespace string, listOpts metav1.ListOptions, handle *WatchHandle) {
	defer func() {
		if r := recover(); r != nil {
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"group":     handle.GroupKind.Group,
				"kind":      handle.GroupKind.Kind,
				"panic":     r,
			}).Error("Panic in watch event processor")
		}
	}()

	minRetry := 1 * time.Second
	maxRetry := 30 * time.Second
	retryInterval := minRetry

	for {
		// Check for cancellation
		select {
		case <-ctx.Done():
			return
		default:
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
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"group":     handle.GroupKind.Group,
				"kind":      handle.GroupKind.Kind,
				"error":     err,
			}).Warnf("Failed to watch resource, retrying in %v", retryInterval)

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

		// Watch established successfully
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
						log.WithFields(log.Fields{
							"component": "graph-cache",
							"group":     handle.GroupKind.Group,
							"kind":      handle.GroupKind.Kind,
						}).Warn("Watch closed unexpectedly, reconnecting...")
						return
					}

					// Update metrics
					handle.LastEventTime = time.Now()
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
func (wm *SelectiveWatchManager) handleEvent(eventType watch.EventType, obj *unstructured.Unstructured) {
	// Check if resource has Argo CD tracking
	trackingInfo := ExtractTrackingInfo(obj, wm.trackingMethod)

	// For POC, we only process resources with tracking
	// In the future, we might also process descendants without tracking
	if !trackingInfo.HasTracking {
		return
	}

	// Call the event handler
	if wm.onResourceEvent != nil {
		wm.onResourceEvent(eventType, obj)
	}
}

// discoverResource discovers the GVR and scope for a GroupKind.
func (wm *SelectiveWatchManager) discoverResource(gk schema.GroupKind) (schema.GroupVersionResource, bool, error) {
	startTime := time.Now()
	defer func() {
		wm.metricsLock.Lock()
		wm.metrics.LastDiscoveryTime = time.Now()
		wm.metrics.DiscoveryDuration = time.Since(startTime)
		wm.metricsLock.Unlock()
	}()

	// Get API resources
	apiResourceLists, err := wm.discoveryClient.ServerPreferredResources()
	if err != nil {
		// Partial errors are common and acceptable
		log.WithError(err).Debug("Partial error during API discovery")
	}

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

// ListManagedResources performs an initial list of all resources with Argo CD tracking.
// This is used for initial discovery before watches are established.
func (wm *SelectiveWatchManager) ListManagedResources(gvr schema.GroupVersionResource, isNamespaced bool) ([]*unstructured.Unstructured, error) {
	listOpts := metav1.ListOptions{}

	// Add label selector for label-based tracking
	if wm.trackingMethod == TrackingMethodLabel || wm.trackingMethod == TrackingMethodAnnotationAndLabel {
		listOpts.LabelSelector = "app.kubernetes.io/instance"
	}

	var result []*unstructured.Unstructured

	if isNamespaced {
		if len(wm.namespaces) == 0 {
			// List across all namespaces
			list, err := wm.dynamicClient.Resource(gvr).List(wm.ctx, listOpts)
			if err != nil {
				return nil, err
			}
			for i := range list.Items {
				result = append(result, &list.Items[i])
			}
		} else {
			// List in specific namespaces
			for _, ns := range wm.namespaces {
				list, err := wm.dynamicClient.Resource(gvr).Namespace(ns).List(wm.ctx, listOpts)
				if err != nil {
					log.WithError(err).WithField("namespace", ns).Warn("Failed to list resources in namespace")
					continue
				}
				for i := range list.Items {
					result = append(result, &list.Items[i])
				}
			}
		}
	} else {
		// Cluster-scoped resource
		list, err := wm.dynamicClient.Resource(gvr).List(wm.ctx, listOpts)
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			result = append(result, &list.Items[i])
		}
	}

	// Filter by tracking method (for annotation-only tracking)
	if wm.trackingMethod == TrackingMethodAnnotation {
		var filtered []*unstructured.Unstructured
		for _, obj := range result {
			if HasArgoTracking(obj, wm.trackingMethod) {
				filtered = append(filtered, obj)
			}
		}
		result = filtered
	}

	return result, nil
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