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
	Namespaces    []string // Empty means cluster-scoped
	Watcher       watch.Interface
	Cancel        context.CancelFunc
	ResourceCount int // Number of resources watched
	StartTime     time.Time
	LastEventTime time.Time
	IsNamespaced  bool
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
// If a watch already exists, this is a no-op. Returns true if a new watch was created.
func (wm *SelectiveWatchManager) EnsureWatch(gk schema.GroupKind) (bool, error) {
	wm.watchLock.Lock()
	defer wm.watchLock.Unlock()

	// Check if watch already exists
	if _, exists := wm.watches[gk]; exists {
		return false, nil
	}

	// Discover resource information
	gvr, isNamespaced, err := wm.discoverResource(gk)
	if err != nil {
		return false, fmt.Errorf("failed to discover resource %s: %w", gk, err)
	}

	// Create watch
	handle, err := wm.createWatch(gk, gvr, isNamespaced)
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
		"kind":       gk.Kind,
		"namespaced": isNamespaced,
		"namespaces": wm.namespaces,
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
	if handle.Watcher != nil {
		handle.Watcher.Stop()
	}

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
		if handle.Watcher != nil {
			handle.Watcher.Stop()
		}
		delete(wm.watches, gk)
	}

	log.WithField("component", "graph-cache").Info("Selective watch manager shutdown complete")
}

// createWatch creates a watch for the specified resource type.
func (wm *SelectiveWatchManager) createWatch(gk schema.GroupKind, gvr schema.GroupVersionResource, isNamespaced bool) (*WatchHandle, error) {
	ctx, cancel := context.WithCancel(wm.ctx)

	handle := &WatchHandle{
		GroupKind:    gk,
		Namespaces:   wm.namespaces,
		Cancel:       cancel,
		StartTime:    time.Now(),
		IsNamespaced: isNamespaced,
	}

	// Build list options with label selector for tracking
	listOpts := metav1.ListOptions{
		Watch: true,
	}

	// For label-based tracking, we can use a label selector
	if wm.trackingMethod == TrackingMethodLabel || wm.trackingMethod == TrackingMethodAnnotationAndLabel {
		// Watch resources with the app.kubernetes.io/instance label
		// Note: This is a basic selector; in production we might want to be more sophisticated
		listOpts.LabelSelector = "app.kubernetes.io/instance"
	}

	// Start watches based on scope
	if isNamespaced {
		if len(wm.namespaces) == 0 {
			// Watch all namespaces
			watcher, err := wm.dynamicClient.Resource(gvr).Watch(ctx, listOpts)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("failed to watch all namespaces: %w", err)
			}
			handle.Watcher = watcher
		} else {
			// For POC, we'll watch the first namespace
			// In production, we'd need to manage multiple watchers
			namespace := wm.namespaces[0]
			watcher, err := wm.dynamicClient.Resource(gvr).Namespace(namespace).Watch(ctx, listOpts)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("failed to watch namespace %s: %w", namespace, err)
			}
			handle.Watcher = watcher
		}
	} else {
		// Cluster-scoped resource
		watcher, err := wm.dynamicClient.Resource(gvr).Watch(ctx, listOpts)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to watch cluster-scoped resource: %w", err)
		}
		handle.Watcher = watcher
	}

	// Start event processing goroutine
	go wm.processEvents(handle)

	return handle, nil
}

// processEvents processes watch events for a specific resource type.
func (wm *SelectiveWatchManager) processEvents(handle *WatchHandle) {
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

	resultChan := handle.Watcher.ResultChan()

	for {
		select {
		case <-wm.ctx.Done():
			return

		case event, ok := <-resultChan:
			if !ok {
				// Watch closed
				log.WithFields(log.Fields{
					"component": "graph-cache",
					"group":     handle.GroupKind.Group,
					"kind":      handle.GroupKind.Kind,
				}).Warn("Watch closed unexpectedly")
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
