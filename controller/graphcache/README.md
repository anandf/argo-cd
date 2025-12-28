# Graph-Based Cache for Argo CD (Proof of Concept)

## Overview

This is a proof-of-concept implementation of a **selective, graph-based cache** for Argo CD that only watches resources managed by Argo CD applications. This approach significantly reduces memory consumption and API server load compared to the traditional cache that watches all resource types in a cluster.

## Problem Statement

The current gitops-engine cache watches **all resource types** in a cluster, leading to:
- High memory consumption
- Unnecessary API server load
- Watching resources that Argo CD doesn't manage

For example, in a cluster with 50+ CRDs, the traditional cache creates watches for all of them, even if Argo CD only manages a handful of applications with 5-10 resource types.

## Solution

The graph-based cache:
1. **Only watches resources managed by Argo CD** (identified via tracking labels/annotations)
2. **Automatically discovers and watches descendant resources** (e.g., Deployment → ReplicaSet → Pod)
3. **Watches CRD instances when the CRD itself is managed**
4. **Uses graph relationships** to efficiently query resource hierarchies

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    GraphCache                            │
├─────────────────────────────────────────────────────────┤
│                                                           │
│  ┌─────────────────┐  ┌──────────────────────────────┐  │
│  │ ResourceGraph   │  │  SelectiveWatchManager       │  │
│  │                 │  │                              │  │
│  │ - Nodes (res)   │  │  - Dynamic watch creation    │  │
│  │ - Edges (rel)   │  │  - Label/annotation filter   │  │
│  │ - Indices       │  │  - Event handling            │  │
│  └─────────────────┘  └──────────────────────────────┘  │
│                                                           │
│  ┌──────────────────────────────────────────────────┐   │
│  │         DescendantTracker                        │   │
│  │                                                   │   │
│  │  - OwnerReference parsing                        │   │
│  │  - Implicit relationships (Service → Endpoints)  │   │
│  │  - Resource type inference                       │   │
│  └──────────────────────────────────────────────────┘   │
│                                                           │
└─────────────────────────────────────────────────────────┘
```

## Key Components

### 1. ResourceGraph (`types.go`)
- Stores all managed resources as nodes with parent-child relationships
- Efficient indexing by application name and resource type
- Thread-safe operations with read/write locks

### 2. SelectiveWatchManager (`watch_manager.go`)
- Dynamically creates watches for discovered resource types
- Filters events based on Argo CD tracking labels/annotations
- Manages watch lifecycle (creation, updates, cleanup)

### 3. DescendantTracker (`descendants.go`)
- Discovers resource relationships:
  - **Explicit**: Via OwnerReferences (Deployment → ReplicaSet → Pod)
  - **Implicit**: Well-known patterns (Service → Endpoints, StatefulSet → PVC)
- Extracts resource references (Pod → Secret, Pod → ConfigMap)

### 4. Tracking Integration (`tracking.go`)
- Supports three tracking methods:
  - **Label**: `app.kubernetes.io/instance`
  - **Annotation**: `argocd.argoproj.io/tracking-id`
  - **Annotation+Label**: Both methods (default)

## Resource Relationships Supported

### Tier 1: Direct Relationships (OwnerReferences)
- Deployment → ReplicaSet → Pod
- StatefulSet → Pod
- DaemonSet → Pod
- Job → Pod
- CronJob → Job → Pod

### Tier 2: Implicit Relationships
- Service → Endpoints
- Service → EndpointSlice
- ServiceAccount → Secret (token secrets)
- StatefulSet → PersistentVolumeClaim

### Tier 3: Resource References
- Pod → Secret (volumes, env vars)
- Pod → ConfigMap (volumes, env vars)
- Deployment/StatefulSet/etc. → Secret/ConfigMap (via PodTemplateSpec)

## Usage

### As a Library

```go
import "github.com/argoproj/argo-cd/v3/controller/graphcache"

// Create graph cache
cache, err := graphcache.NewGraphCache(graphcache.Config{
    DynamicClient:   dynamicClient,
    DiscoveryClient: discoveryClient,
    TrackingMethod:  graphcache.TrackingMethodAnnotationAndLabel,
    Namespaces:      []string{"default", "production"}, // or nil for all
})

// Start the cache (performs initial discovery)
err = cache.Start()

// Query resources
resources := cache.GetResourcesByApplication("guestbook")
children := cache.GetChildren(deploymentKey)
metrics := cache.GetMetrics()

// Cleanup
cache.Shutdown()
```

### Demo CLI

A demonstration CLI tool is provided to see the cache in action:

```bash
# Build the demo
cd controller/graphcache/cmd/demo
go build -o graph-cache-demo

# Run with default settings (all namespaces, annotation+label tracking)
./graph-cache-demo

# Run with specific namespace
./graph-cache-demo -namespace=argocd

# Run with label-only tracking
./graph-cache-demo -tracking-method=label

# Debug mode
./graph-cache-demo -log-level=debug -duration=120
```

**Demo Output:**
```
Graph Cache POC Demo
===================
INFO Creating graph cache tracking_method=annotation+label namespaces=[]
INFO Graph cache started successfully

Cache Metrics:
  Total Managed Resources: 42
  Active Watches: 8
  Unique Resource Types: 8
  Unique Applications: 3
  Total Events: 156 (Add: 42, Update: 112, Delete: 2)
  Last Discovery: 14:32:15 (took 1.2s)
  Resources Discovered: 42
  Descendant Types Added: 4

Resources by Type:
  apps/Deployment: 5
  apps/ReplicaSet: 5
  core/Pod: 15
  core/Service: 8
  core/ConfigMap: 3
  core/Secret: 4
  core/Endpoints: 8
  networking.k8s.io/Ingress: 2

Resources by Application:
  guestbook: 15 resources
  helm-guestbook: 18 resources
  kustomize-app: 9 resources

Active Watches:
  apps/Deployment
  apps/ReplicaSet
  core/Pod
  core/Service
  core/ConfigMap
  core/Secret
  core/Endpoints
  networking.k8s.io/Ingress

Comparison:
  Estimated Traditional Cache Watches: 42
  Graph Cache Watches: 8
  Reduction: 81%
```

## Metrics and Evaluation

### Primary Metrics

```go
type CacheMetrics struct {
    // Resource counts
    TotalManagedResources   int
    ResourcesByType         map[schema.GroupKind]int
    ResourcesByApplication  map[string]int

    // Watch counts
    ActiveWatches           int
    WatchesByType           map[schema.GroupKind]bool

    // Discovery metrics
    DiscoveryRuns           int
    LastDiscoveryTime       time.Time
    LastDiscoveryDuration   time.Duration
    ResourcesDiscovered     int
    DescendantTypesAdded    int

    // Performance
    AverageEventProcessTime time.Duration
}
```

### Expected Results

For a typical Argo CD installation managing 5-10 applications:

| Metric | Traditional Cache | Graph Cache | Improvement |
|--------|------------------|-------------|-------------|
| Active Watches | 40-50 | 8-15 | 60-80% reduction |
| Memory Usage | ~500MB | ~150MB | 70% reduction |
| API Events/sec | High | Low | Fewer unnecessary events |

## Testing

Unit tests are provided for core functionality:

```bash
cd controller/graphcache
go test -v ./...
```

**Test Coverage:**
- ✅ Resource graph operations (add, update, delete, query)
- ✅ Parent-child relationship management
- ✅ Application and type indexing
- ✅ Concurrent access safety
- ✅ Metrics calculation

**TODO (Future):**
- Integration tests with real Kubernetes cluster
- Performance benchmarks
- E2E tests with Argo CD applications

## Configuration

### Environment Variables

```bash
# Enable graph cache (future integration)
export ARGOCD_GRAPH_CACHE_ENABLED=true

# Tracking method
export ARGOCD_GRAPH_CACHE_TRACKING_METHOD=annotation+label

# Discovery interval (seconds)
export ARGOCD_GRAPH_CACHE_DISCOVERY_INTERVAL=300

# Log level
export ARGOCD_GRAPH_CACHE_LOG_LEVEL=info
```

### ConfigMap (Future)

```yaml
# argocd-cm ConfigMap
data:
  graphcache.enabled: "true"
  graphcache.tracking.method: "annotation+label"
  graphcache.discovery.interval: "5m"
```

## Limitations (POC)

This is a proof-of-concept with the following limitations:

1. **Single namespace watch**: For namespaced resources, only watches the first namespace in the list (production would watch all specified namespaces)
2. **No persistence**: All state is in-memory; full rebuild on restart
3. **Basic error handling**: Production would need comprehensive retry logic and error recovery
4. **No CRD instance auto-discovery**: When a CRD is managed, we don't yet automatically watch instances
5. **Simple label selector**: Uses basic label selector; production might need more sophisticated filtering
6. **No metrics export**: Metrics are only available via API; not yet exported to Prometheus

## Next Steps

### If POC is Successful (>50% watch reduction)

1. **Hardening**
   - Comprehensive error handling
   - Watch reconnection logic
   - Edge case handling
   - Resilience testing

2. **Optimization**
   - Multi-namespace watch support
   - Efficient graph traversal algorithms
   - Memory optimization
   - Query performance tuning

3. **Integration**
   - Integrate cyphernetes library for advanced queries
   - Prometheus metrics export
   - Integration with existing LiveStateCache
   - Feature flag for gradual rollout

4. **Testing**
   - Integration tests with real clusters
   - Performance benchmarks vs traditional cache
   - Load testing with many applications
   - E2E tests

5. **Production Readiness**
   - Documentation
   - Migration guide
   - Monitoring and alerting
   - SLOs and SLIs

### If POC Shows Limited Improvement (<30% reduction)

1. Analyze why (tracking coverage, relationship discovery, etc.)
2. Identify gaps in discovery logic
3. Consider hybrid approach (selective + traditional)
4. Document findings and recommendations

## Design Documentation

See [DESIGN.md](./DESIGN.md) for detailed design documentation, including:
- Architecture diagrams
- Component specifications
- Workflow descriptions
- Resource type relationships
- Implementation plan

## References

- [Argo CD Resource Tracker](https://github.com/anandf/resource-tracker) - Reference implementation
- [Cyphernetes Library](https://github.com/anandf/cyphernetes) - Graph query library
- [GitOps Engine Cache](https://github.com/argoproj/gitops-engine/tree/master/pkg/cache) - Current cache implementation
- [Kubernetes Owner References](https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/)

## License

Apache 2.0 (same as Argo CD)
