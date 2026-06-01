---
title: Selective Resource Watching for Argo CD Application Controller
authors:
  - "@anandf"
sponsors:
  - TBD
reviewers:
  - TBD
approvers:
  - TBD

creation-date: 2026-05-28
last-updated: 2026-05-28
---

# Selective Resource Watching for Argo CD Application Controller

Argo CD's application controller currently watches **all** resource types in every managed cluster. This proposal introduces selective resource watching — an opt-in alpha feature that watches only the resource types actually used by Argo CD applications, reducing memory consumption and Kubernetes API server load by 60–80%.

## Open Questions

- Should selective watching eventually become the default cache implementation, or should it remain opt-in permanently?
> TBD — this depends on production validation and community feedback during the alpha phase.

- Should `resources.exclusions` and `resources.inclusions` from `argocd-cm` be honored by the selective watch mechanism?
> Yes, exclusions should be respected as an additional filter. Inclusions are less relevant since the selective watch already limits scope, but should be honored for consistency.

- How should CRD auto-discovery work? Should the system watch all CRDs in a cluster, or only CRDs that appear in application manifests?
> Only CRDs referenced in application manifests should be watched. When a new CRD is observed via an OwnerReference on a managed resource, a watch is created on demand.

- What is the fallback behavior when manifest discovery fails (e.g., repo server unavailable)?
> Graceful degradation: existing watches are maintained, and the system relies on runtime learning via OwnerReferences. A warning alert is raised in the health check.

- Should learned type relationships be shared across controller replicas in a sharded deployment?
> The ConfigMap-based persistence store enables this implicitly — all replicas read from and write to the same ConfigMap. Conflict resolution uses last-writer-wins semantics since relationships are additive.

## Summary

The traditional Argo CD cluster cache discovers every API resource type in a cluster and creates a Kubernetes watch for each one. In a typical cluster this results in 40–50 active watches, most of which track resource types that Argo CD never manages. The selective resource watching feature replaces this universal approach with a targeted one: watches are created only for resource types that appear in application manifests, plus their known descendants (e.g., Deployment → ReplicaSet → Pod). New resource type relationships are learned at runtime from OwnerReferences and persisted so they survive controller restarts.

This is an **alpha** feature, disabled by default, controlled via the `ARGOCD_ENABLE_GRAPH_CACHE` environment variable.

## Motivation

At scale, the traditional cache architecture creates significant overhead:

- **Memory waste**: Each watch maintains a local in-memory store of all resources of that type. Watching 50 resource types when only 8–15 are relevant wastes 60–80% of cache memory.
- **API server load**: Every watch establishes a long-lived HTTP/2 stream to the Kubernetes API server. Unnecessary watches contribute to API server connection pressure, especially in multi-cluster deployments with 100+ clusters.
- **Irrelevant event processing**: Events from unrelated resource types are received, decoded, and discarded. This consumes CPU cycles that could be spent on reconciliation.
- **Slow startup**: The initial `LIST` call for every resource type delays cache population. Listing only relevant types reduces time-to-ready.

The root of the problem is in `gitops-engine/pkg/cache/cluster.go`, where `startMissingWatches()` calls `GetAPIResources()` to discover **all** APIs and creates a watch for each:

```go
// gitops-engine/pkg/cache/cluster.go:657
func (c *clusterCache) startMissingWatches() error {
    apis, err := c.kubectl.GetAPIResources(c.config, true, c.settings.ResourcesFilter)
    // ... creates a watch for EVERY api resource type
}
```

### Goals

- Watch only resource types that appear in Argo CD application manifests and their known descendants.
- Automatically discover descendant resource types (e.g., Deployment → ReplicaSet → Pod) using pre-seeded Kubernetes knowledge and runtime learning from OwnerReferences.
- Persist learned type relationships across controller restarts via a pluggable store (ConfigMap or file).
- Maintain full compatibility with the existing `LiveStateCache` interface — zero changes to the application controller's reconciliation, sync, or diff logic.
- Provide the feature behind an opt-in feature flag at alpha quality.
- Expose Prometheus metrics to validate memory and watch count improvements.

### Non-Goals

- Replacing the traditional cache as the default implementation (alpha stage — this is a future decision based on production feedback).
- Changing the sync, diff, or health assessment logic.
- Modifying the Application CRD, AppProject CRD, or any Argo CD API types.
- Supporting advanced graph query languages (e.g., Cyphernetes) — this may come in a future proposal.
- Modifying how cluster credentials are managed or how cluster secrets are watched.

## Proposal

### Architecture Overview

The implementation is split across two Go modules to maintain the existing dependency boundaries:

```
┌─────────────────────────────────────────────────────────────────┐
│  gitops-engine/pkg/graphcache/  (core primitives, no argo-cd   │
│                                  dependency)                    │
│                                                                 │
│  ┌─────────────────────┐  ┌──────────────────────────────────┐ │
│  │  SelectiveWatch     │  │  TypeRelationshipCache           │ │
│  │  Manager            │  │                                  │ │
│  │  - Dynamic watch    │  │  - Pre-seeded K8s patterns       │ │
│  │    creation         │  │  - Runtime learning from         │ │
│  │  - Namespace-aware  │  │    OwnerReferences               │ │
│  │  - Failure recovery │  │  - Confidence scoring            │ │
│  └─────────────────────┘  └──────────────────────────────────┘ │
│                                                                 │
│  ┌─────────────────────┐  ┌──────────────────────────────────┐ │
│  │  DescendantTracker  │  │  Persistence (ConfigMap / File)  │ │
│  │  - OwnerRef parsing │  │  - RelationshipStore interface   │ │
│  │  - Implicit rels    │  │  - ConfigMapStore (default)      │ │
│  │    (Svc→Endpoints)  │  │  - FileStore (alternative)      │ │
│  └─────────────────────┘  └──────────────────────────────────┘ │
│                                                                 │
│  ┌────────────────────────────────────────────────────────────┐ │
│  │  ResourceGraph + Tracking + Logging                       │ │
│  └────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
                              │
                    imports via Go module
                              │
┌─────────────────────────────────────────────────────────────────┐
│  controller/graphcache/  (Argo CD integration layer)           │
│                                                                 │
│  ┌─────────────────────┐  ┌──────────────────────────────────┐ │
│  │  GraphCache         │  │  ManifestDiscovery               │ │
│  │  - Orchestrates all │  │  - Fetches manifests from        │ │
│  │    components       │  │    repo server                   │ │
│  │  - Event routing    │  │  - Extracts GVKs                 │ │
│  │  - Health checks    │  │  - Expands with descendants      │ │
│  └─────────────────────┘  └──────────────────────────────────┘ │
│                                                                 │
│  ┌─────────────────────┐  ┌──────────────────────────────────┐ │
│  │  GraphLiveState     │  │  Factory                         │ │
│  │  CacheAdapter       │  │  - Feature flag evaluation       │ │
│  │  - Implements       │  │  - Traditional vs selective      │ │
│  │    LiveStateCache   │  │    cache decision                │ │
│  │  - Zero controller  │  │  - Dependency injection          │ │
│  │    changes needed   │  │                                  │ │
│  └─────────────────────┘  └──────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

**Key design principle**: The `LiveStateCache` interface (`controller/cache/cache.go`) is the contract between the application controller and the cache layer. The selective watch implementation provides a drop-in adapter that satisfies this interface, so the controller's reconciliation loop, sync logic, and diff calculations require zero modifications.

### Two-Phase Resource Discovery

The selective watch mechanism uses a two-phase discovery strategy to solve the bootstrap problem (watches are needed before resources exist, but resource types are unknown before watches are created).

#### Phase 1: Manifest-Based Discovery (Proactive)

Before an application is synced, the `ManifestDiscovery` component fetches the application's manifests from the repo server and extracts the GVKs (GroupVersionKinds) that will be created:

```
Application sync requested
    │
    ▼
ManifestDiscovery.DiscoverFromApplication(app)
    │
    ├── Fetch manifests from repo server (GenerateManifest RPC)
    ├── Parse YAML → extract GVKs (Deployment, Service, ConfigMap, etc.)
    ├── Detect referenced resources in Pod specs:
    │   ├── ConfigMap volumes, Secret volumes
    │   ├── envFrom configMapRef / secretRef
    │   └── env valueFrom configMapKeyRef / secretKeyRef
    ├── Expand with known descendants via TypeRelationshipCache:
    │   ├── Deployment → ReplicaSet → Pod
    │   ├── Service → Endpoints, EndpointSlice
    │   └── StatefulSet → Pod, PersistentVolumeClaim
    │
    ▼
Create watches for ALL discovered types in app's destination namespace
```

This ensures watches are in place **before** the sync creates resources, so all events are captured from the start.

#### Phase 2: Runtime Learning (Reactive)

As resources are created and observed via watches, the system learns new type relationships:

```
New resource observed via watch
    │
    ├── Has OwnerReference? → Learn parent-child type relationship
    │   Example: CustomResource "Foo" creates "Bar"
    │   → TypeRelationshipCache.LearnRelationship(Foo→Bar, confidence+1)
    │
    ├── Matches implicit relationship pattern?
    │   ├── Endpoints with same name as Service → Service→Endpoints
    │   ├── EndpointSlice with service label → Service→EndpointSlice
    │   └── Secret with ServiceAccount annotation → SA→Secret
    │
    └── Persist learned relationships (every 5 minutes)
        → ConfigMap "argocd-graph-cache-relationships" in argocd namespace
```

Learned relationships survive controller restarts and are immediately available when the controller starts next time, eliminating the cold-start learning period.

### Use cases

#### Use case 1:

As an operator managing a small number of applications (e.g., 5 applications deploying Deployments, Services, and ConfigMaps), I would like the application controller to only watch the ~8 relevant resource types instead of all ~50 types in my cluster, so that I can reduce the controller's memory footprint by 60–80%.

#### Use case 2:

As a platform team managing Argo CD across 100+ clusters, I would like to reduce the number of Kubernetes API watch connections per cluster, so that the API servers experience less connection pressure and the overall platform is more stable.

#### Use case 3:

As a user with clusters containing many CRDs (e.g., Istio, Crossplane, cert-manager), I would like the controller to only watch CRDs that are actually referenced in my application manifests, so that dozens of irrelevant CRD watches are not consuming memory and API bandwidth.

#### Use case 4:

As a user adding a new application, I would like the controller to discover the required resource types from the application's Git manifests and create watches proactively, so that all resource events are captured from the very first sync.

### Design Considerations

- **Why a new cache implementation instead of filtering the existing one?** The traditional cache in `gitops-engine/pkg/cache/` is built around the assumption that all resource types are watched. Its `startMissingWatches()` function, event batching, and resource resolution logic all depend on having a complete view of the cluster. Adding selective filtering to this architecture would require invasive changes to a critical code path shared by all gitops-engine consumers.

- **Why split across gitops-engine and controller?** The core graph primitives (`ResourceGraph`, `SelectiveWatchManager`, `TypeRelationshipCache`, etc.) have no dependency on Argo CD types and belong in the gitops-engine alongside the traditional cache. The integration layer (`ManifestDiscovery`, `GraphLiveStateCache` adapter) depends on Argo CD types (`Application`, `AppProject`, repo server client) and must stay in the controller. This split avoids circular dependencies between the two Go modules.

- **Why ConfigMap for persistence instead of Redis?** ConfigMaps are available in every Kubernetes cluster without additional infrastructure. The relationship data is small (typically <100 entries, well within the 1MB ConfigMap limit) and is only written every 5 minutes. Redis can be used via a custom `RelationshipStore` implementation for deployments that already have Redis.

### Implementation Details/Notes/Constraints

#### Component: gitops-engine (Core Primitives)

Located in `gitops-engine/pkg/graphcache/`, these components have no Argo CD dependencies:

| Component | File | Purpose |
|-----------|------|---------|
| `SelectiveWatchManager` | `watch_manager.go` | Creates and manages per-type Kubernetes watches with namespace awareness, failure recovery (max 10 consecutive failures), and API discovery caching (2-minute TTL) |
| `TypeRelationshipCache` | `type_relationships.go` | Pre-seeded with well-known Kubernetes parent-child patterns; learns new patterns from OwnerReferences with confidence scoring |
| `DescendantTracker` | `descendants.go` | Extracts parent-child relationships from OwnerReferences and implicit patterns (Service→Endpoints, SA→Secret) |
| `ResourceGraph` | `types.go` | Thread-safe graph data structure storing resource nodes with parent-child edges, indexed by application name and resource type |
| `TrackingInfo` | `tracking.go` | Extracts Argo CD application ownership from labels (`app.kubernetes.io/instance`) and annotations (`argocd.argoproj.io/tracking-id`) |
| `RelationshipStore` | `persistence_interface.go` | Pluggable interface for persisting learned type relationships |
| `ConfigMapStore` | `configmap_store.go` | Default persistence implementation using a Kubernetes ConfigMap |
| `FileStore` | `file_store.go` | Alternative persistence implementation using a local file |

**Pre-seeded type relationships** (confidence: 100):

| Parent | Child |
|--------|-------|
| apps/v1 Deployment | apps/v1 ReplicaSet |
| apps/v1 ReplicaSet | v1 Pod |
| apps/v1 StatefulSet | v1 Pod |
| apps/v1 StatefulSet | v1 PersistentVolumeClaim |
| apps/v1 DaemonSet | v1 Pod |
| batch/v1 Job | v1 Pod |
| batch/v1 CronJob | batch/v1 Job |
| v1 Service | v1 Endpoints |
| v1 Service | discovery.k8s.io/v1 EndpointSlice |

#### Component: Argo CD Application Controller (Integration)

Located in `controller/graphcache/`, these components integrate with Argo CD:

| Component | File | Purpose |
|-----------|------|---------|
| `GraphCache` | `graph_cache.go` | Main orchestrator: wires all components together, handles events, manages lifecycle, exposes health checks and metrics |
| `ManifestDiscovery` | `manifest_discovery.go` | Proactive GVK discovery: fetches manifests from repo server, parses them, extracts GVKs and referenced resources (Secrets/ConfigMaps from Pod specs) |
| `GraphLiveStateCache` | `adapter.go` | Implements `LiveStateCache` interface for transparent integration with the application controller; manages per-cluster graph cache instances |
| `Factory` | `factory.go` | Feature flag evaluation (`ARGOCD_ENABLE_GRAPH_CACHE`), decides between traditional and selective cache, dependency injection |

#### Component: Configuration

The feature is controlled via environment variables on the application controller:

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `ARGOCD_ENABLE_GRAPH_CACHE` | `false` | Master switch to enable selective watching |
| `ARGOCD_GRAPH_CACHE_TRACKING_METHOD` | `""` (uses settings manager) | Override tracking method: `annotation`, `label`, or `annotation+label` |
| `ARGOCD_RELATIONSHIP_STORE_PATH` | `""` (uses ConfigMap) | File path for relationship persistence; when set, uses file store instead of ConfigMap |
| `ARGOCD_CUSTOM_RELATIONSHIPS_FILE` | `""` | Path to a YAML file defining custom parent-child type relationships |

**Tunable Parameters** (via `GraphConfig`):

| Parameter | Default | Description |
|-----------|---------|-------------|
| `ShardCount` | 32 | Number of graph shards for concurrent access |
| `DiscoveryInterval` | 5m | Interval for periodic resource type re-discovery |
| `MaxConsecutiveFailures` | 100 | Max watch failures before stopping a watch goroutine |
| `MinRetryInterval` | 1s | Minimum backoff for watch reconnection |
| `MaxRetryInterval` | 30s | Maximum backoff for watch reconnection |
| `MetricsExportInterval` | 30s | Interval for exporting Prometheus metrics |

### Detailed examples

#### Example 1: Enabling selective resource watching

Set the environment variable on the application controller deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: argocd-application-controller
  namespace: argocd
spec:
  template:
    spec:
      containers:
      - name: argocd-application-controller
        env:
        - name: ARGOCD_ENABLE_GRAPH_CACHE
          value: "true"
```

After restarting the controller, log output will confirm:

```
INFO Graph cache enabled — traditional gitops-engine cache will NOT be initialized
INFO Graph cache initialized — cluster caches will use selective watches only  trackingMethod=annotation+label namespaces=[argocd]
```

#### Example 2: Monitoring watch reduction via Prometheus metrics

Once enabled, the following metrics are available at the controller's `/metrics` endpoint:

```
# Number of active watches (compare with ~50 for traditional cache)
argocd_graph_cache_watched_types 8

# Total managed resources in cache
argocd_graph_cache_total_resources 142

# Number of applications tracked
argocd_graph_cache_applications 5

# Estimated cache memory in bytes
argocd_graph_cache_memory_bytes 15728640

# Watch events processed
argocd_graph_cache_watch_events_total{type="ADDED"} 142
argocd_graph_cache_watch_events_total{type="MODIFIED"} 87
argocd_graph_cache_watch_events_total{type="DELETED"} 12
```

#### Example 3: Defining custom type relationships

For operators or CRDs that create child resources not discoverable via standard OwnerReferences, define a custom relationships file:

```yaml
# /etc/argocd/custom-relationships.yaml
relationships:
  - parent:
      group: "stable.example.com"
      version: "v1"
      kind: "CronTab"
    child:
      group: ""
      version: "v1"
      kind: "Pod"
  - parent:
      group: "cert-manager.io"
      version: "v1"
      kind: "Certificate"
    child:
      group: ""
      version: "v1"
      kind: "Secret"
```

Mount the file and set the environment variable:

```yaml
env:
- name: ARGOCD_CUSTOM_RELATIONSHIPS_FILE
  value: "/etc/argocd/custom-relationships.yaml"
volumeMounts:
- name: custom-relationships
  mountPath: /etc/argocd
volumes:
- name: custom-relationships
  configMap:
    name: argocd-custom-relationships
```

#### Example 4: Using file-based persistence instead of ConfigMap

For environments where writing to ConfigMaps is restricted:

```yaml
env:
- name: ARGOCD_RELATIONSHIP_STORE_PATH
  value: "/data/relationships.json"
volumeMounts:
- name: relationship-data
  mountPath: /data
volumes:
- name: relationship-data
  emptyDir: {}
```

#### Example 5: Verifying the health of the selective watch cache

The graph cache exposes a health check API that reports:

```json
{
  "healthy": true,
  "totalResources": 142,
  "activeWatches": 8,
  "memoryUsageMB": 15,
  "alerts": [],
  "lastDiscoveryTime": "2026-05-28T10:30:00Z"
}
```

When unhealthy, alerts indicate the issue:

```json
{
  "healthy": false,
  "alerts": [
    {"severity": "critical", "message": "No active watches established"},
    {"severity": "warning", "message": "Discovery stale: last run 15m ago"}
  ]
}
```

### Security Considerations

- **No privilege escalation**: Selective watches use the same Kubernetes service account as the traditional cache. No additional RBAC permissions are required.
- **Reduced attack surface**: By caching fewer resource types, the controller holds less sensitive data in memory. A compromised controller has access to fewer resources than with the traditional cache.
- **Relationship persistence**: Learned relationships are stored in a ConfigMap (`argocd-graph-cache-relationships`) in the `argocd` namespace. This ConfigMap is protected by the same RBAC as other Argo CD ConfigMaps (`argocd-cm`, `argocd-rbac-cm`). The data contains only type-level relationship metadata (e.g., "Deployment creates ReplicaSet"), not resource instances or secrets.
- **Custom relationships file**: When using `ARGOCD_CUSTOM_RELATIONSHIPS_FILE`, the file should be mounted read-only from a trusted source (ConfigMap or Secret). Malicious entries could cause unnecessary watches but cannot grant access beyond the controller's existing RBAC.

### Risks and Mitigations

#### Risk: Missing watches for resource types not in manifests

If a resource type is created by a controller or operator that is not explicitly declared in the application manifest, the selective watch may not have a watch for it initially.

**Mitigation**: Runtime learning via OwnerReferences. When a parent resource (which is watched) creates a child resource, the OwnerReference on the child triggers a new watch for the child's type. Additionally, the `resources.inclusions` setting can be used to force-watch specific types. The custom relationships file provides a static escape hatch for known gaps.

#### Risk: Manifest discovery failure

If the repo server is unavailable when an application is first processed, manifest-based discovery cannot run.

**Mitigation**: Graceful degradation. The system falls back to runtime learning only. Existing watches are maintained. A warning alert is raised in the health check. On the next reconciliation cycle, manifest discovery is retried.

#### Risk: Cross-namespace ownership not captured

Kubernetes OwnerReferences only work within a single namespace. Cluster-scoped resources owning namespaced resources (e.g., a ClusterRole referenced by a RoleBinding) are not captured by OwnerReference-based learning.

**Mitigation**: The `DescendantTracker` includes implicit relationship patterns for known cross-scope relationships. The custom relationships file allows operators to define additional cross-scope relationships.

#### Risk: Goroutine leaks from failing watches

A watch that repeatedly fails (e.g., due to RBAC changes) could leak a goroutine if not properly managed.

**Mitigation**: The `SelectiveWatchManager` enforces a maximum consecutive failure count (default: 100). After exceeding this threshold, the watch goroutine is stopped and the watch is removed. The failure is logged and surfaced in health check alerts.

### Upgrade / Downgrade Strategy

#### Upgrade

- **No configuration changes required**: The feature is disabled by default. Existing clusters continue to use the traditional cache with no changes.
- **To opt in**: Set `ARGOCD_ENABLE_GRAPH_CACHE=true` on the application controller and restart. No CRD changes, no API changes, no data migration.
- **Gradual rollout**: In a sharded deployment, enable the feature on a single controller replica first by setting the environment variable only on that pod.

#### Downgrade

- **Set `ARGOCD_ENABLE_GRAPH_CACHE=false`** (or remove the variable) and restart the controller. The traditional cache resumes immediately.
- **No data cleanup needed**: The `argocd-graph-cache-relationships` ConfigMap can be left in place — it is ignored when the feature is disabled. It can also be safely deleted.

#### Compatibility

- This feature does not modify any CRDs, API types, or ConfigMap schemas used by other components.
- The `LiveStateCache` interface contract is fully preserved — the rest of the controller (sync, diff, health, UI resource tree) works identically regardless of which cache implementation is active.

## Drawbacks

- **Additional complexity**: The caching layer now has two implementations. Both must be maintained and tested. The selective watch code adds ~3,000 lines of Go across gitops-engine and controller.
- **Cold start learning period**: On first startup with no persisted relationships, the system relies solely on pre-seeded Kubernetes patterns and manifest discovery. Custom CRD relationships must be learned through observation or configured via the custom relationships file.
- **Potential for incomplete coverage**: If manifest discovery misses a resource type and runtime learning has not yet observed it, the cache may not have a watch for it. The controller falls back to direct API calls in this case, but this is slower than a cache hit.
- **Testing surface**: Two cache implementations means the full test matrix doubles. E2E tests must validate both paths.

## Alternatives

### Alternative 1: Add label-based filtering to the traditional cache

Modify the traditional cache's `startMissingWatches()` to use label selectors on watches, filtering for only Argo CD-managed resources.

**Why rejected**: Kubernetes watches with label selectors still require server-side processing. More importantly, child resources (ReplicaSets, Pods) typically do not carry the Argo CD tracking label — they are linked via OwnerReferences, not labels. This approach would miss all descendant resources.

### Alternative 2: Use `resources.inclusions` / `resources.exclusions` aggressively

Configure `resources.inclusions` in `argocd-cm` to list only the resource types used by applications, effectively reducing the traditional cache's scope.

**Why rejected**: This requires manual configuration and must be updated whenever application manifests change. It is error-prone (forgetting a type breaks sync), does not handle descendant resources automatically, and does not adapt to new applications or CRDs without operator intervention.

### Alternative 3: Reduce watch scope via informer-based filtering

Use Kubernetes informers with field selectors or transform functions to reduce the amount of data stored per watch, without reducing the number of watches.

**Why rejected**: This reduces memory per watch but does not reduce the number of API server connections or the initial LIST call overhead. The fundamental problem is too many watches, not too much data per watch.

## Related Issues

- https://github.com/argoproj/argo-cd/issues/7489 — Resource tracking improvements
- https://github.com/argoproj/argo-cd/issues/12509 — High memory consumption in large clusters

## Related Links

- [gitops-engine cluster cache](https://github.com/argoproj/gitops-engine/tree/master/pkg/cache) — Traditional cache implementation
- [Kubernetes API Concepts: Efficient detection of changes](https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes) — Watch API documentation
