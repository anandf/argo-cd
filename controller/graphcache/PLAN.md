# Graph-Based Cache: Epics & Stories

## Epic 1: Core Graph Data Structure

**Goal:** Build the foundational in-memory graph that stores resources with parent-child relationships and efficient indexing.

### Story 1.1 -- Resource Graph with Sharded Storage
Implement `ResourceGraph` struct with sharded maps (configurable shard count, default 32). Each node stores a `kube.ResourceKey` -> resource metadata mapping. Support concurrent read/write via per-shard `sync.RWMutex`.

**Key types:** `ResourceNode`, `ResourceGraph`, `GraphConfig`
**Operations:** `AddOrUpdate`, `Delete`, `Get`, `GetByKey`

### Story 1.2 -- Parent-Child Relationship Tracking
Maintain `parentUID -> []childKey` index built from `OwnerReferences`. When a resource is added/updated, parse its `OwnerReferences` to establish edges. When deleted, clean up both directions.

**Operations:** `GetChildren(key)`, `GetParent(key)`, `IterateHierarchy(key, action)`

### Story 1.3 -- Application Index
Maintain an index from `appName -> []ResourceKey` using the existing resource tracking system (`util/argo/resource_tracking.go`). Support all three tracking methods (label, annotation, annotation+label). Extract app name from resources using `ResourceTracking.GetAppName()`.

**Operations:** `GetResourcesByApplication(appName)`, `GetApplicationForResource(key)`

### Story 1.4 -- Type Index (GVK -> Resources)
Index resources by `schema.GroupVersionKind` so the cache can quickly answer "all Deployments" or "all Pods" queries. This is needed for `IterateResources` and API resource discovery.

### Story 1.5 -- Label Index with Delta Updates
Index resources by labels for efficient filtering. Use delta-based updates (only update changed labels) rather than full replace to minimize lock contention.

---

## Epic 2: Selective Watch Manager

**Goal:** Dynamically create/destroy Kubernetes watches for only the resource types managed by Argo CD applications, filtered by tracking labels/annotations.

### Story 2.1 -- Watch Lifecycle Manager
Implement `SelectiveWatchManager` that creates `dynamic.Informer` watches per GVR. Track active watches, handle start/stop, and support adding new GVRs at runtime. Use label selectors (`app.kubernetes.io/instance` exists) or annotation-based filtering in event handlers.

### Story 2.2 -- Event Handler Pipeline
Wire watch events (Add/Update/Delete) into the `ResourceGraph`. On Add/Update: parse resource, extract tracking info, update graph. On Delete: remove from graph and clean up relationships. Must be thread-safe and non-blocking.

### Story 2.3 -- Watch Failure Resilience
Implement retry with exponential backoff (configurable min/max intervals). Cap consecutive failures (default 100) before stopping a watch to prevent goroutine leaks. Log watch health metrics.

### Story 2.4 -- Watch Cleanup on Resource Type Removal
When no resources of a given GVK are managed anymore, stop the corresponding watch after a grace period. Prevents watch accumulation over time.

---

## Epic 3: Resource Type Discovery

**Goal:** Discover which Kubernetes resource types need watches, using two complementary strategies.

### Story 3.1 -- Well-Known Type Relationship Seed Data
Seed a `TypeRelationshipCache` with known Kubernetes patterns:
- `Deployment -> ReplicaSet -> Pod`
- `StatefulSet -> Pod, PVC`
- `Job -> Pod`, `CronJob -> Job`
- `Service -> Endpoints, EndpointSlice`

Each seeded relationship has confidence=100.

### Story 3.2 -- Runtime Relationship Learning
When a resource with an `OwnerReference` is observed, learn the parent->child GVK relationship. Increment confidence on repeated observations. This handles CRDs and operator-created resources automatically.

### Story 3.3 -- Manifest-Based Discovery (Proactive)
Before sync, call the repo server's `GetManifests` to extract GVKs from application manifests. Expand with known descendants from the `TypeRelationshipCache`. Create watches before resources exist -- solves the bootstrap problem.

**Dependency:** Requires repo server client (`reposerver.Clientset`).

### Story 3.4 -- API Discovery Integration
Use the Kubernetes discovery client to resolve GVK->GVR mappings and determine if resources are namespaced. Cache discovery results with periodic refresh (configurable interval, default 5m).

---

## Epic 4: LiveStateCache Adapter

**Goal:** Implement the `LiveStateCache` interface (`controller/cache/cache.go:134-157`) so the graph cache is a drop-in replacement for the gitops-engine cache.

This is the critical integration epic -- every method must behave identically to the existing implementation.

### Story 4.1 -- `GetManagedLiveObjs`
Return live objects managed by an application, matching by resource key against target objects. Must handle:
- Resources tracked by label vs annotation
- Cluster-scoped vs namespaced resources
- Shared resource detection (resource managed by a different app)

The existing implementation is in `controller/cache/cache.go` and delegates to `ClusterCache.GetManagedLiveObjs`. The graph cache version should query the graph's application index instead.

### Story 4.2 -- `IterateHierarchyV2`
Given a set of `ResourceKey`s, traverse the parent->child tree and invoke the callback for each node. Must produce `appv1.ResourceNode` with health status, resource version, and parent refs. Must support the `action` returning `false` to prune subtrees.

### Story 4.3 -- `IterateResources`
Iterate all cached resources for a given cluster, producing `clustercache.Resource` + `ResourceInfo` enrichment (health, images, pod info, network info). This is used by the controller to build the full resource tree.

### Story 4.4 -- `GetVersionsInfo` and `IsNamespaced`
Delegate to the Kubernetes discovery client. Cache results. `IsNamespaced` must be thread-safe (this was a known race condition in early designs).

### Story 4.5 -- `GetClusterCache` Compatibility Shim
Some controller code calls `GetClusterCache()` to access `ClusterCache` methods directly. Either implement a thin adapter that wraps graph cache operations in the `ClusterCache` interface, or identify all call sites and ensure they work through the `LiveStateCache` interface instead.

### Story 4.6 -- `Run`, `Init`, `UpdateShard`
- `Init()`: Load persisted relationships, seed type cache, perform initial API discovery
- `Run(ctx)`: Start watch manager, discovery loops, metrics export, persistence goroutines
- `UpdateShard(shard)`: Integrate with cluster sharding -- only watch clusters assigned to this shard

### Story 4.7 -- `GetNamespaceTopLevelResources`
Return resources in a namespace that have no `OwnerReferences`. Used for orphaned resource detection. Query the graph for resources where parent is nil.

### Story 4.8 -- `GetClustersInfo`
Return cache statistics per cluster (resource count, watch count, last sync time, errors). Map from graph cache metrics.

---

## Epic 5: Multi-Cluster Support

**Goal:** Support multiple destination clusters, matching the existing LiveStateCache behavior.

### Story 5.1 -- Per-Cluster Graph Cache Instances
Maintain a map of `serverURL -> GraphCache`. Create/destroy instances as clusters are added/removed from the Argo CD cluster registry.

### Story 5.2 -- Cluster Sharding Integration
Respect `ClusterShardingCache.IsManagedCluster()` -- only create graph cache instances for clusters assigned to this controller shard. Handle shard reassignment by stopping/starting watches.

### Story 5.3 -- Cluster Credential Refresh
Watch for cluster secret changes (connection credentials rotate). Recreate dynamic clients and watches when credentials change, without losing cached state.

---

## Epic 6: Persistence

**Goal:** Persist learned type relationships across pod restarts so the cache doesn't need to re-learn from scratch.

### Story 6.1 -- Persistence Interface
Define `RelationshipStore` interface with `Save`, `Load`, `Close`, `GetStats` methods. Keep it pluggable.

### Story 6.2 -- ConfigMap Store (Default)
Persist relationships to a ConfigMap (`argocd-graph-cache-relationships`). Handle 1MB limit with confidence-based truncation (drop lowest confidence relationships first). Auto-save on interval (default 5m) and on graceful shutdown.

### Story 6.3 -- Redis Store (Optional)
Implement Redis-backed store for environments that already run Redis. No size limit concerns. Shared across controller replicas.

---

## Epic 7: Observability

**Goal:** Expose metrics, health checks, and structured logging for production operations.

### Story 7.1 -- Prometheus Metrics
Register metrics with the existing `controller/metrics` package:
- `argocd_graph_cache_total_resources` (gauge, per cluster)
- `argocd_graph_cache_watched_types` (gauge, per cluster)
- `argocd_graph_cache_watch_events_total` (counter, by type and event kind)
- `argocd_graph_cache_query_duration_seconds` (histogram)
- `argocd_graph_cache_memory_bytes` (gauge)

Export on configurable interval (default 30s).

### Story 7.2 -- Health Check Endpoint
Implement `HealthCheck()` returning structured status: healthy/unhealthy, alert list with severity, key metrics. Integrate with the server's `/healthz` endpoint for Kubernetes probes.

### Story 7.3 -- Structured Logging
Use logrus fields consistently: `component=graph-cache`, `cluster=<server>`, `operation=<op>`. Add duration logging for key operations. No correlation IDs needed initially -- keep it simple.

---

## Epic 8: Feature Flag & Rollout

**Goal:** Enable safe, gradual adoption in production environments.

### Story 8.1 -- Environment Variable Feature Flag
Gate all graph cache code behind `ARGOCD_ENABLE_GRAPH_CACHE=true` in `appcontroller.go`. When disabled (default), use the existing `NewLiveStateCache` path. Zero impact on existing behavior.

### Story 8.2 -- ConfigMap-Based Configuration
Support `argocd-cm` keys for runtime configuration:
- `graphcache.enabled`
- `graphcache.tracking.method`
- `graphcache.persistence.store` (configmap | redis)

### Story 8.3 -- Gradual Rollout Strategy (Post-MVP)
Support per-cluster or percentage-based rollout via `argocd-cm` configuration. Use hash-based stable selection so the same cluster consistently uses the same cache backend.

---

## Epic 9: Testing

**Goal:** Validate correctness, performance, and resilience.

### Story 9.1 -- Unit Tests for Core Graph
Test all `ResourceGraph` operations: CRUD, hierarchy traversal, index queries, concurrent access (race detector). Target: >90% coverage of core types.

### Story 9.2 -- Unit Tests for Adapter
Test each `LiveStateCache` method against known resource sets. Compare output with the existing implementation to ensure behavioral parity.

### Story 9.3 -- Integration Tests with Fake Clients
Use `k8s.io/client-go/dynamic/fake` to test the watch manager, discovery, and event pipeline end-to-end without a real cluster.

### Story 9.4 -- Benchmark Tests
Compare memory usage, watch count, and query latency between graph cache and traditional cache for synthetic workloads (10, 100, 1000 applications).

### Story 9.5 -- E2E Tests
Add test cases in `test/e2e/` that enable graph cache via env var and run standard sync scenarios (guestbook, multi-app, auto-sync, cascade delete, Helm, Kustomize).

### Story 9.6 -- Stress & Chaos Tests (Post-MVP)
1000+ apps, 10k+ resources, concurrent updates, API server failures, watch flapping.

---

## Recommended Implementation Order

| Phase | Epics | Goal |
|-------|-------|------|
| **Phase 1: Foundation** | Epic 1, Epic 2, Epic 3 (3.1, 3.4) | Core graph + selective watches working |
| **Phase 2: Integration** | Epic 4, Epic 5 (5.1) | Drop-in replacement for LiveStateCache |
| **Phase 3: Production Readiness** | Epic 6 (6.1, 6.2), Epic 7, Epic 8 (8.1) | Observable, persistent, feature-flagged |
| **Phase 4: Validation** | Epic 9 (9.1-9.5) | Confidence to deploy |
| **Phase 5: Rollout** | Epic 3 (3.2, 3.3), Epic 5 (5.2, 5.3), Epic 8 (8.2, 8.3), Epic 9 (9.6) | Gradual production adoption |

---

## Key Risks & Decisions

1. **`GetClusterCache()` compatibility** (Story 4.5) -- Some controller code bypasses `LiveStateCache` and calls `ClusterCache` directly. Need to audit all call sites to determine if a full `ClusterCache` adapter is needed or if the interface can be narrowed.

2. **Behavioral parity** -- The existing cache has subtle behaviors around resource enrichment (`ResourceInfo` -- health, pod info, network info). The graph cache must produce identical `ResourceInfo` structures or the UI resource tree will break.

3. **Manifest discovery dependency** (Story 3.3) -- Requires repo server client, which introduces a new dependency for the controller cache layer. This should be optional (graceful degradation to runtime-only learning).

4. **Multi-namespace watches** -- The current design docs mention "single namespace watch" as a limitation. The real implementation must watch across all application-namespaces, matching the existing cache behavior.

5. **CRD resources** -- When an Application manages a CRD itself, the cache needs to dynamically watch instances of that CRD. This requires re-discovery after sync completes.
