package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Graph cache metrics for monitoring performance and resource usage
var (
	// graphCacheTotalResources tracks the total number of resources in the graph cache
	graphCacheTotalResources = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "argocd_graph_cache_total_resources",
			Help: "Total number of resources tracked in the graph cache",
		},
		[]string{"server"},
	)

	// graphCacheWatchedTypes tracks the number of resource types being watched
	graphCacheWatchedTypes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "argocd_graph_cache_watched_types",
			Help: "Number of resource types (GVKs) being watched by the graph cache",
		},
		[]string{"server"},
	)

	// graphCacheApplications tracks the number of applications managed
	graphCacheApplications = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "argocd_graph_cache_applications",
			Help: "Number of applications tracked in the graph cache",
		},
		[]string{"server"},
	)

	// graphCacheMemoryBytes tracks memory usage of the graph cache
	graphCacheMemoryBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "argocd_graph_cache_memory_bytes",
			Help: "Memory usage of the graph cache in bytes",
		},
		[]string{"server"},
	)

	// graphCacheWatchEvents tracks watch events processed
	graphCacheWatchEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "argocd_graph_cache_watch_events_total",
			Help: "Total number of watch events processed by the graph cache",
		},
		[]string{"server", "event_type", "gvk"},
	)

	// graphCacheQueryDuration tracks query execution time
	graphCacheQueryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "argocd_graph_cache_query_duration_seconds",
			Help:    "Duration of graph cache queries",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		},
		[]string{"server", "query_type"},
	)

	// graphCacheManifestDiscovery tracks manifest discovery operations
	graphCacheManifestDiscovery = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "argocd_graph_cache_manifest_discovery_total",
			Help: "Total number of manifest discovery operations",
		},
		[]string{"server", "status"},
	)

	// graphCachePersistenceOperations tracks persistence save/load operations
	graphCachePersistenceOperations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "argocd_graph_cache_persistence_operations_total",
			Help: "Total number of persistence operations (save/load)",
		},
		[]string{"operation", "status"},
	)

	// graphCacheRelationshipsLearned tracks learned resource relationships
	graphCacheRelationshipsLearned = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "argocd_graph_cache_relationships_learned",
			Help: "Number of resource type relationships learned by the graph cache",
		},
		[]string{"confidence_level"},
	)

	// graphCacheResourceRefreshes tracks resource refresh operations
	graphCacheResourceRefreshes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "argocd_graph_cache_resource_refreshes_total",
			Help: "Total number of resource refresh operations",
		},
		[]string{"server", "gvk", "status"},
	)
)

// RegisterGraphCacheMetrics registers all graph cache metrics with the provided registry
func RegisterGraphCacheMetrics(registry *prometheus.Registry) {
	registry.MustRegister(graphCacheTotalResources)
	registry.MustRegister(graphCacheWatchedTypes)
	registry.MustRegister(graphCacheApplications)
	registry.MustRegister(graphCacheMemoryBytes)
	registry.MustRegister(graphCacheWatchEvents)
	registry.MustRegister(graphCacheQueryDuration)
	registry.MustRegister(graphCacheManifestDiscovery)
	registry.MustRegister(graphCachePersistenceOperations)
	registry.MustRegister(graphCacheRelationshipsLearned)
	registry.MustRegister(graphCacheResourceRefreshes)
}

// GraphCacheMetrics provides methods to update graph cache metrics
type GraphCacheMetrics struct{}

// NewGraphCacheMetrics creates a new GraphCacheMetrics instance and registers
// all graph cache metrics with the provided registry. If registry is nil,
// metrics are not registered.
func NewGraphCacheMetrics(registry *prometheus.Registry) *GraphCacheMetrics {
	if registry != nil {
		RegisterGraphCacheMetrics(registry)
	}
	return &GraphCacheMetrics{}
}

// SetTotalResources sets the total number of resources in cache
func (m *GraphCacheMetrics) SetTotalResources(server string, count int) {
	graphCacheTotalResources.WithLabelValues(server).Set(float64(count))
}

// SetWatchedTypes sets the number of watched resource types
func (m *GraphCacheMetrics) SetWatchedTypes(server string, count int) {
	graphCacheWatchedTypes.WithLabelValues(server).Set(float64(count))
}

// SetApplications sets the number of tracked applications
func (m *GraphCacheMetrics) SetApplications(server string, count int) {
	graphCacheApplications.WithLabelValues(server).Set(float64(count))
}

// SetMemoryBytes sets the memory usage in bytes
func (m *GraphCacheMetrics) SetMemoryBytes(server string, bytes int64) {
	graphCacheMemoryBytes.WithLabelValues(server).Set(float64(bytes))
}

// IncWatchEvent increments the watch event counter
func (m *GraphCacheMetrics) IncWatchEvent(server, eventType, gvk string) {
	graphCacheWatchEvents.WithLabelValues(server, eventType, gvk).Inc()
}

// ObserveQueryDuration records a query duration
func (m *GraphCacheMetrics) ObserveQueryDuration(server, queryType string, duration float64) {
	graphCacheQueryDuration.WithLabelValues(server, queryType).Observe(duration)
}

// IncManifestDiscovery increments the manifest discovery counter
func (m *GraphCacheMetrics) IncManifestDiscovery(server, status string) {
	graphCacheManifestDiscovery.WithLabelValues(server, status).Inc()
}

// IncPersistenceOperation increments the persistence operation counter
func (m *GraphCacheMetrics) IncPersistenceOperation(operation, status string) {
	graphCachePersistenceOperations.WithLabelValues(operation, status).Inc()
}

// SetRelationshipsLearned sets the number of learned relationships
func (m *GraphCacheMetrics) SetRelationshipsLearned(confidenceLevel string, count int) {
	graphCacheRelationshipsLearned.WithLabelValues(confidenceLevel).Set(float64(count))
}

// IncResourceRefresh increments the resource refresh counter
func (m *GraphCacheMetrics) IncResourceRefresh(server, gvk, status string) {
	graphCacheResourceRefreshes.WithLabelValues(server, gvk, status).Inc()
}
