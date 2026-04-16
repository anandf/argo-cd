# Graph Cache: Gap Analysis & Index Design

## Gaps in the Current Implementation

### Gap 1: No Code Exists

The `controller/graphcache/` directory contains only `ROADMAP.md` and `IMPROVEMENTS.md`. All Go code described in CLAUDE.md (types.go, graph_cache.go, watch_manager.go, adapter.go, etc.) does not exist. The documentation describes aspirational architecture, not implemented code.

### Gap 2: Missing Indexes That the GitOps-Engine Cache Relies On

The existing gitops-engine `clusterCache` uses three core indexes that every `LiveStateCache` method depends on:

| Index | Type | Used By |
|-------|------|---------|
| `resources` | `map[ResourceKey]*Resource` | Every method -- global O(1) lookup by group/kind/namespace/name |
| `nsIndex` | `map[string]map[ResourceKey]*Resource` | `IterateHierarchyV2`, `GetNamespaceTopLevelResources`, `buildGraph`, `getApp` |
| `parentUIDToChildren` | `map[UID][]ResourceKey` | Cross-namespace hierarchy traversal, `iterateChildrenUsingIndex` |

The CLAUDE.md design mentions an "application index" and "type index" but does not mention the `nsIndex` or `parentUIDToChildren` index -- both of which are critical for behavioral parity.

### Gap 3: ResourceInfo Enrichment Pipeline Missing

The existing cache populates a `ResourceInfo` struct on every resource via `PopulateResourceInfoHandler`. This enrichment includes:
- Health status (via `health.GetResourceHealth` + Lua overrides)
- App name (via `ResourceTracking.GetAppName`)
- Container images (from Pod specs)
- Networking info (Ingress URLs, Service selectors, Pod IPs)
- Pod info (node name, resource requests, phase)
- Node info (capacity, kernel version)
- Manifest hash (for `skipResourceUpdate` optimization)

The graph cache design documents don't address how this enrichment pipeline will be replicated.

### Gap 4: `GetClusterCache()` Returns `ClusterCache` Interface Directly

The controller calls `GetClusterCache()` at two critical sites:
- `state.go:658` -- to get a `ResourceInfoProvider` (for `IsNamespaced`)
- `appcontroller.go:836` -- to get `GetGVKParser()` for building diff configs

This means the graph cache adapter must either:
1. Implement the full `ClusterCache` interface (50+ methods), or
2. Return a thin shim that only implements the methods actually called

### Gap 5: `GetManagedLiveObjs` Fallback to API Server

The existing implementation falls back to `kubectl.GetResource()` when a resource isn't in the cache or when an API type isn't watched. Since the graph cache watches fewer types by design, this fallback path becomes more important and must be preserved.

### Gap 6: Inferred Owner References

The gitops-engine handles implicit parent-child relationships beyond `OwnerReferences`:
- `StatefulSet -> PVC` (via VolumeClaimTemplate naming pattern)
- `Endpoints -> Service` (implicit ownership)
- `Secret -> ServiceAccount` (token secrets)
- `ClusterServiceVersion -> OperatorGroup` (via annotation)

These are resolved in `resolveResourceReferences()` and stored via `isInferredParentOf` callbacks. The graph cache design documents mention "implicit relationships" but don't address this specific mechanism.

### Gap 7: `buildGraph()` Within-Namespace Graph Construction

For `IterateHierarchyV2`, the gitops-engine builds a per-namespace graph (`map[ResourceKey]map[UID]*Resource`) from `OwnerReferences` for within-namespace traversal, separate from the `parentUIDToChildren` index used for cross-namespace traversal. The graph cache needs to replicate this two-phase traversal strategy.

### Gap 8: Manifest Caching Decision

The gitops-engine only caches full `unstructured.Unstructured` manifests for resources that are app-managed or CRDs (returned by the `PopulateResourceInfoHandler` boolean). The graph cache CLAUDE.md says "store full objects" -- but indiscriminately caching all manifests will increase memory usage, defeating the purpose.

### Gap 9: Event-Driven App Requeuing

When a resource changes, the existing cache walks up the owner chain (`getAppRecursive`) to find the managing application, then triggers `onObjectUpdated(toNotify, ref)` to requeue the app for reconciliation. This includes:
- Circular dependency detection in owner chains
- `skipAppRequeuing` for high-churn resources (Endpoints)
- `skipResourceUpdate` based on manifest hash comparison

### Gap 10: OpenAPI Schema and GVK Parser

The existing `ClusterCache` maintains an OpenAPI schema and `GvkParser` (for structured merge diff). The controller accesses this via `GetClusterCache().GetGVKParser()`. The graph cache needs to provide this or the diff/secret-hiding logic breaks.

---

## Index-Based Storage Design

### Primary Indexes (Required for LiveStateCache parity)

```
+---------------------------------------------------------------------+
|                    GraphCache Index Structure                         |
+---------------------------------------------------------------------+
|                                                                       |
|  INDEX 1: Global Resource Map                                        |
|  ----------------------------                                        |
|  resources: map[kube.ResourceKey]*GraphNode                          |
|                                                                       |
|  Purpose: O(1) lookup of any resource by group/kind/ns/name         |
|  Used by: Every method                                               |
|  Sharding: Hash ResourceKey across N shards, each with own          |
|            RWMutex to reduce contention                              |
|                                                                       |
|  struct GraphNode {                                                  |
|      ResourceKey      kube.ResourceKey                               |
|      Ref              corev1.ObjectReference  // UID, APIVersion     |
|      OwnerRefs        []metav1.OwnerReference                        |
|      Resource         *unstructured.Unstructured  // only if managed |
|      ResourceVersion  string                                         |
|      CreationTimestamp *metav1.Time                                   |
|      Info             *ResourceInfo  // health, images, networking   |
|      AppName          string         // cached from tracking         |
|  }                                                                   |
|                                                                       |
+---------------------------------------------------------------------+
|                                                                       |
|  INDEX 2: Namespace Index                                            |
|  ------------------------                                            |
|  nsByNamespace: map[string]map[kube.ResourceKey]*GraphNode           |
|                                                                       |
|  Purpose: O(1) get all resources in a namespace                      |
|  Used by:                                                            |
|    - IterateHierarchyV2 (buildGraph per namespace)                   |
|    - GetNamespaceTopLevelResources (filter OwnerRefs==0)             |
|    - getApp (recursive owner chain walk within namespace)            |
|    - IterateResources (when filtered by namespace)                   |
|  Note: nsByNamespace[""] holds cluster-scoped resources              |
|                                                                       |
+---------------------------------------------------------------------+
|                                                                       |
|  INDEX 3: Parent UID -> Children                                     |
|  -------------------------------                                     |
|  parentUIDToChildren: map[types.UID][]kube.ResourceKey               |
|                                                                       |
|  Purpose: O(1) lookup of all direct children of a resource           |
|  Used by:                                                            |
|    - IterateHierarchyV2 (cross-namespace traversal)                  |
|    - iterateChildrenUsingIndex (recursive DFS)                       |
|  Maintenance:                                                        |
|    - On Add: append child key to parent's UID entry                  |
|    - On Delete: remove child key (swap-and-pop)                      |
|    - On Update: diff old/new OwnerRefs, update accordingly           |
|    - Full rebuild after initial sync                                 |
|                                                                       |
+---------------------------------------------------------------------+
|                                                                       |
|  INDEX 4: Application -> Resources                                   |
|  ---------------------------------                                   |
|  appToResources: map[string]sets.Set[kube.ResourceKey]               |
|                                                                       |
|  Purpose: O(1) get all root resources managed by an application      |
|  Used by:                                                            |
|    - GetManagedLiveObjs (find managed resources by app name)         |
|    - IterateHierarchyV2 (starting keys come from managed list)       |
|  Maintenance:                                                        |
|    - On Add/Update: if AppName set, add to set                       |
|    - On Delete: remove from set                                      |
|    - On AppName change: move between sets                            |
|  Note: Only root resources (OwnerRefs==0) go in this index           |
|                                                                       |
+---------------------------------------------------------------------+
|                                                                       |
|  INDEX 5: GVK -> Resources (Selective Watch Targeting)               |
|  -----------------------------------------------------               |
|  resourcesByGVK: map[schema.GroupVersionKind]sets.Set[ResourceKey]    |
|                                                                       |
|  Purpose: Track which resource types are in cache                    |
|  Used by:                                                            |
|    - Watch manager: decide which GVRs to watch/unwatch               |
|    - IterateResources: filter by type                                |
|    - Discovery: track which types have managed instances             |
|  Maintenance:                                                        |
|    - On Add: insert into GVK set                                     |
|    - On Delete: remove; if set empty, candidate for unwatch          |
|                                                                       |
+---------------------------------------------------------------------+
|                                                                       |
|  INDEX 6: API Scope Cache                                            |
|  ------------------------                                            |
|  namespacedResources: map[schema.GroupKind]bool                      |
|                                                                       |
|  Purpose: O(1) check if a resource type is namespaced                |
|  Used by: IsNamespaced                                               |
|  Source: Kubernetes discovery API, refreshed periodically            |
|  Locking: Must be thread-safe (known race in early design)           |
|                                                                       |
+---------------------------------------------------------------------+
|                                                                       |
|  INDEX 7: Watch State                                                |
|  --------------------                                                |
|  activeWatches: map[schema.GroupVersionResource]*WatchState           |
|                                                                       |
|  struct WatchState {                                                 |
|      Cancel       context.CancelFunc                                 |
|      GVR          schema.GroupVersionResource                        |
|      Namespaced   bool                                               |
|      StartedAt    time.Time                                          |
|      LastEventAt  time.Time                                          |
|      EventCount   int64                                              |
|      Failures     int                                                |
|  }                                                                   |
|                                                                       |
|  Purpose: Track watch lifecycle and health                           |
|  Used by: Watch manager, health checks, metrics                      |
|                                                                       |
+---------------------------------------------------------------------+
```

### How Each LiveStateCache Method Maps to Indexes

```
Method                        Indexes Used                          Traversal Pattern
---------------------------------------------------------------------------------------------------------
GetManagedLiveObjs            appToResources -> resources           App name -> root keys -> full objects
                                                                    Fallback: kubectl.GetResource for
                                                                    unwatched types

IterateHierarchyV2            resources + nsByNamespace             Group keys by namespace
                              + parentUIDToChildren                 Phase 1: buildGraph(nsNodes) for
                                                                             within-NS
                                                                    Phase 2: parentUIDToChildren for
                                                                             cross-NS

GetNamespaceTopLevelResources nsByNamespace                         Filter: len(OwnerRefs) == 0

IterateResources              resources (all)                       Full scan with ResourceInfo extraction

IsNamespaced                  namespacedResources                   O(1) map lookup

GetVersionsInfo               discovery client cache                Delegate to K8s API

GetClusterCache               (shim returning graph cache adapter)  Must expose GetGVKParser()
```

### Hierarchy Traversal Algorithm (matching gitops-engine behavior)

```
IterateHierarchyV2(keys):
    visited = {}

    // Phase 1: Group by namespace
    keysByNS = groupByNamespace(keys)

    // Phase 2: Within-namespace traversal
    for ns, nsKeys in keysByNS:
        nsNodes = nsByNamespace[ns]
        graph = buildGraphFromOwnerRefs(nsNodes)   // map[key]map[uid]*node
        for key in nsKeys:
            if visited[key]: continue
            node = resources[key]
            if !action(node): continue
            visited[key] = VISITING
            dfsChildren(node, graph, nsNodes, visited, action)
            visited[key] = DONE

    // Phase 3: Cross-namespace (cluster-scoped -> namespaced children)
    if clusterKeys = keysByNS[""]:
        for key in clusterKeys:
            node = resources[key]
            childKeys = parentUIDToChildren[node.UID]
            for childKey in childKeys:
                if visited[childKey] && childKey.NS != "": continue
                child = resources[childKey]
                nsNodes = nsByNamespace[childKey.NS]
                if action(child):
                    visited[childKey] = VISITING
                    dfsChildren(child, nsNodes, visited, action)
                    // Recurse for cluster-scoped children
                    if childKey.NS == "":
                        crossNamespaceTraversal(childKey, visited, action)
                    visited[childKey] = DONE
```

### Sharding Strategy for Index 1 (Global Resource Map)

```
+--------------------------------------------------+
|              Sharded Resource Map                 |
|                                                    |
|  shardFor(key) = hash(key.Namespace + key.Name    |
|                       + key.Group + key.Kind) % N  |
|                                                    |
|  shard[0]: { RWMutex, map[ResourceKey]*GraphNode } |
|  shard[1]: { RWMutex, map[ResourceKey]*GraphNode } |
|  ...                                               |
|  shard[N]: { RWMutex, map[ResourceKey]*GraphNode } |
|                                                    |
|  N = GraphConfig.ShardCount (default 32)           |
+--------------------------------------------------+
```

**Important design note on sharding:** The gitops-engine uses a single `RWMutex` protecting all three indexes together. This is critical -- `IterateHierarchyV2` must see a consistent snapshot across `resources`, `nsIndex`, and `parentUIDToChildren` simultaneously. If we shard the resource map independently from the hierarchy indexes, we risk inconsistent reads during traversal.

The practical design should be:

- **Option A (recommended for correctness):** Single `RWMutex` for all indexes (matches gitops-engine). Sharding only applies if we partition by cluster (which we already do -- one `GraphCache` per cluster).
- **Option B (for throughput):** Shard the resource map for writes, but acquire a global read lock for traversal methods. This adds complexity for marginal benefit since reads already use `RLock`.

### What's Unique to the Graph Cache (not in gitops-engine)

| Index | Purpose | Why Graph Cache Needs It |
|-------|---------|--------------------------|
| `appToResources` | App->ResourceKeys | Avoids full scan in `GetManagedLiveObjs` -- gitops-engine scans all resources with `isManaged()` predicate; graph cache can do O(1) lookup |
| `resourcesByGVK` | GVK->ResourceKeys | Drives selective watching -- when a GVK set becomes empty, stop the watch |
| `activeWatches` | GVR->WatchState | Track which watches are running, their health, and when to clean up |

These three indexes are what make the graph cache selective rather than watching everything. The gitops-engine doesn't need them because it watches all types unconditionally.

---

## Index Maintenance on CRUD Operations

### On Resource Add

```
1. resources[key] = node                              // Index 1
2. nsByNamespace[ns][key] = node                       // Index 2
3. for ownerRef in node.OwnerRefs:                     // Index 3
       parentUIDToChildren[ownerRef.UID].append(key)
4. if node.AppName != "" && len(OwnerRefs) == 0:       // Index 4
       appToResources[appName].add(key)
5. resourcesByGVK[gvk].add(key)                        // Index 5
```

### On Resource Update

```
1. old = resources[key]
2. resources[key] = node                               // Index 1
3. nsByNamespace[ns][key] = node                        // Index 2
4. diff old.OwnerRefs vs new.OwnerRefs:                 // Index 3
       removed UIDs: remove key from parentUIDToChildren[uid]
       added UIDs: append key to parentUIDToChildren[uid]
5. if old.AppName != new.AppName:                       // Index 4
       appToResources[old.AppName].remove(key)
       appToResources[new.AppName].add(key)
6. if old.GVK != new.GVK:                              // Index 5
       resourcesByGVK[old.GVK].remove(key)
       resourcesByGVK[new.GVK].add(key)
```

### On Resource Delete

```
1. delete resources[key]                               // Index 1
2. delete nsByNamespace[ns][key]                        // Index 2
3. for ownerRef in node.OwnerRefs:                     // Index 3
       parentUIDToChildren[ownerRef.UID].remove(key)
4. if node.AppName != "":                              // Index 4
       appToResources[appName].remove(key)
5. resourcesByGVK[gvk].remove(key)                     // Index 5
   if resourcesByGVK[gvk].empty():
       // candidate for watch cleanup after grace period
```

---

## Complexity Summary

| Operation | Complexity | Notes |
|-----------|------------|-------|
| Get resource by key | O(1) | Direct map lookup |
| Get resources by namespace | O(1) lookup, O(n_ns) iterate | Via nsIndex |
| Get children of resource | O(1) lookup, O(k) iterate | Via parentUIDToChildren, k = num children |
| Get resources by app | O(1) lookup, O(m) iterate | Via appToResources, m = num root resources |
| Get resources by GVK | O(1) lookup, O(p) iterate | Via resourcesByGVK |
| Check if namespaced | O(1) | Via namespacedResources |
| IterateHierarchyV2 | O(n_ns) per namespace | buildGraph is O(n_ns), DFS is O(nodes visited) |
| GetNamespaceTopLevelResources | O(n_ns) | Scan namespace, filter OwnerRefs==0 |
| GetManagedLiveObjs | O(m) | m = managed root resources for the app |