---
title: Server Side Pagination for Applications List and Watch APIs
authors:
  - "@alexmt"
  - "@anandf"
sponsors:
  - "@jessesuen"
reviewers:
  - TBD
approvers:
  - TBD

creation-date: 2024-02-14
last-updated: 2026-04-16
---

# Introduce Server Side Pagination for Applications List and Watch APIs

Improve Argo CD performance by introducing server side pagination for Applications List and Watch APIs.

## Open Questions

- Should server-side filters (health, sync status, cluster, namespace) be implemented in the same phase as pagination, or as a follow-up?
  > Recommended as a follow-up. Pagination alone provides the highest-impact improvement. Server-side filters complement pagination but can be layered on independently.

- How should the status bar (health/sync breakdown) display accurate totals when only a page of data is loaded?
  > An `ApplicationListStats` field in the List response provides aggregate counts computed over the full RBAC-filtered dataset, not just the current page.

- Should `continue`-token (cursor-based) pagination be supported alongside `offset`/`limit`?
  > Yes. `continue` tokens provide stable pagination under concurrent modifications. `offset`/`limit` is simpler for UIs. Both are supported; `continue` takes precedence when both are provided.

- How does caching interact with pagination to avoid redundant RBAC evaluation?
  > A per-user cache of RBAC-filtered application names (keyed by `resourceVersion`) allows page 2/3/4 requests to skip the RBAC evaluation loop entirely. See Phase 2 below.

## Summary

The Argo CD API server currently returns all applications in a single response. This can be a performance
bottleneck when there are a large number of applications. This proposal is to introduce server-side pagination
for the Applications List and Watch APIs.
Further improvements like RBAC result caching, namespace-scoped filtering is also scoped in to reduce per-request payload and eliminate redundant work.

## Motivation

The main motivation for this proposal it to improve the Argo CD UI responsiveness when there are a large number
of applications. The API server memory usage increases with the number of applications however this is not critical
and can be mitigated by increasing memory limits for the API server deployment. The UI however becomes unresponsive
even on a powerful machine when the number of applications increases 2000. The server side pagination will allow
to reduce amount of data returned by the API server and improve the UI responsiveness.

The specific bottlenecks are:

1. **Serialization cost**:
Serializing large number of Application objects (each ~5-10KB) per List request costs 50-200ms of CPU time and produces multi-megabyte responses.

2. **RBAC evaluation overhead**:
Every List request evaluates `s.enf.Enforce()` against every application in the informer cache.
For large number of apps with complex RBAC policies, this costs 5-50ms per request -- even for page 2/3/4 requests that could reuse results from page 1.

3. **Client-side memory and rendering**:
The UI loads all applications into browser memory, then paginates client-side. With 2000+ apps, this causes multi-second JavaScript pauses for parsing, filtering, and rendering.

4. **Watch initial sync**:
The Watch endpoint sends ADDED events for all applications during initial connection, regardless of namespace filtering, creating a burst of data on connection.

### Assumptions

- The informer-based cache (`SharedIndexInformer`) will continue to be the source of truth for Application objects in the API server. Direct Kubernetes API calls for pagination (using native `limit`/`continue`) are not appropriate because Argo CD's RBAC layer operates above the Kubernetes API.
- The Watch endpoint continues to stream all events in real-time. Pagination applies only to the initial List response and subsequent page requests.
- Existing clients (CLI, CI/CD integrations, custom scripts) that do not send pagination parameters will continue to receive the full application list unchanged.

### Goals

- Support server-side pagination for the Applications List API using `limit`, `offset`, and `continue` (cursor-based) parameters.
- Introduce per-user RBAC result caching so that multi-page browsing does not re-evaluate RBAC against all applications for each page request.
- Fix the Watch endpoint to respect `appNamespace` filtering during initial ADDED event sync.
- Expose `appNamespace` as a UI filter to enable server-side namespace scoping.
- Leverage pagination in the Argo CD CLI to reduce API server load.
- Provide aggregate application statistics (total count, health/sync breakdowns) alongside paginated responses.

### Non-Goals

- Server-side filtering by health status, sync status, cluster, namespace, repo, or search text. These are valuable follow-ups but are not required for pagination to provide significant improvement. The existing proposal's filter fields (`repos`, `clusters`, `namespaces`, `healthStatuses`, `syncStatuses`, `search`, `autoSyncEnabled`) are preserved for future implementation.
- Modifying the Watch endpoint to support paginated streaming. Watch continues to stream all matching events.
- Addressing API server CPU usage from large RBAC policy evaluation. While RBAC caching mitigates repeated evaluation, the initial RBAC evaluation cost is not reduced.

## Proposal

The implementation is organized into three phases, each independently deployable and providing incremental value.

### Phase 1: Namespace-Scoped Server-Side Filtering

**Goal:** Reduce data volume by filtering applications by namespace server-side before they reach the client.

#### Watch Endpoint Bug Fix

The Watch handler ignores the `appNamespace` parameter when sending initial ADDED events. It calls `s.appLister.List(selector)` for all namespaces, while the List handler correctly scopes to the requested namespace.

**Current behavior (buggy):**
```go
// Watch handler sends ALL apps as initial ADDED events
apps, err := s.appLister.List(selector)
```

**Fixed behavior:**
```go
// Scope initial ADDED events to the requested namespace
if q.GetAppNamespace() != "" && appNs != s.ns {
    apps, err = s.appLister.Applications(appNs).List(selector)
} else {
    apps, err = s.appLister.List(selector)
}
```

This mirrors the List handler pattern and requires no proto changes since `appNamespace` is already a field in `ApplicationQuery` (proto field 7).

#### UI: App Namespace Filter

Expose `appNamespace` as a dropdown filter in the applications list UI.
When selected, both the List and Watch API calls include the namespace parameter, reducing the dataset at the source.

No backend changes needed beyond the Watch bug fix -- the List API already accepts `appNamespace`.

### Phase 2: RBAC-Filtered Name Caching

**Goal:** Cache the list of application names a user is allowed to see, so that page 2/3/4 requests skip RBAC evaluation entirely.

#### Why This Is Needed

Even with pagination, the server must determine which applications a user can see before slicing a page.
Without caching, every page request re-evaluates RBAC on all 1000+ applications:

```
Page 1: Informer(1000 apps) → RBAC filter(1000 checks) → Sort → Slice [0:50]   = 50 apps
Page 2: Informer(1000 apps) → RBAC filter(1000 checks) → Sort → Slice [50:100] = 50 apps  ← redundant!
Page 3: Informer(1000 apps) → RBAC filter(1000 checks) → Sort → Slice [100:150] = 50 apps ← redundant!
```

With caching:
```
Page 1: Informer → RBAC filter → Cache 800 allowed names → Slice [0:50]     = 50 apps
Page 2: Cache hit → Fetch 50 apps by name from informer                      = 50 apps ← no RBAC!
Page 3: Cache hit → Fetch 50 apps by name from informer                      = 50 apps ← no RBAC!
```

#### Cache Design

Cache only the **sorted list of qualified application names** that pass RBAC, not full objects.
This keeps cache entries small and avoids stale data issues since objects are always fetched fresh from the informer.

**Cache key:** `app-list|{username}|{resourceVersion}|{namespace}|{selector}|{projects}`

Including `resourceVersion` from `s.appInformer.LastSyncResourceVersion()` provides natural invalidation.
When any Application is added, modified, or deleted, the resourceVersion changes, and old cache entries miss automatically (expiring via TTL).

**TTL:** 15 seconds for name lists, 10 seconds for summary counts.

**Implementation in `server/cache/cache.go`:**

```go
type AppListSummary struct {
    TotalCount   int64            `json:"totalCount"`
    HealthCounts map[string]int64 `json:"healthCounts"`
    SyncCounts   map[string]int64 `json:"syncCounts"`
}

func (c *Cache) GetAllowedAppNames(userHash, resourceVersion, namespace, selector string, projects []string) ([]string, error)
func (c *Cache) SetAllowedAppNames(userHash, resourceVersion, namespace, selector string, projects []string, names []string) error
func (c *Cache) GetAppListSummary(userHash, resourceVersion, namespace, selector string, projects []string) (*AppListSummary, error)
func (c *Cache) SetAppListSummary(userHash, resourceVersion, namespace, selector string, projects []string, summary *AppListSummary) error
```

**Integration in `server/application/application.go` List() handler:**

```go
username := session.Username(ctx)
resourceVersion := s.appInformer.LastSyncResourceVersion()

// Try cache first
cachedNames, err := s.cache.GetAllowedAppNames(username, resourceVersion, q.GetAppNamespace(), q.GetSelector(), projects)
if err == nil && q.Name == nil && q.GetRepo() == "" {
    // Cache hit: fetch only named apps from informer — no RBAC evaluation
    newItems = s.fetchAppsByNames(cachedNames)
} else {
    // Cache miss: run full RBAC loop, then cache the result
    newItems = s.listAppsWithRBACFilter(ctx, q, selector, projects)
    names := extractQualifiedNames(newItems)
    _ = s.cache.SetAllowedAppNames(username, resourceVersion, q.GetAppNamespace(), q.GetSelector(), projects, names)
}
```

#### RBAC Policy Invalidation

RBAC policies are stored in the `argocd-rbac-cm` ConfigMap.
When policies change, the cache key's `resourceVersion` doesn't change (it tracks Application objects, not ConfigMaps).
To ensure immediate invalidation, the cache key includes the RBAC `policyVersion` — a monotonically increasing counter maintained by the `Enforcer`
that increments on every policy change (`SetUserPolicy`, `SetBuiltinPolicy`, or runtime policy updates).
This closes the security window where stale RBAC permissions could be served from cache.

```go
// Cache key includes policyVersion for immediate RBAC invalidation
cachedNames, err := s.cache.GetAllowedAppNames(
    username, resourceVersion, s.enf.PolicyVersion(),
    q.GetAppNamespace(), q.GetSelector(), projects)
```

### Phase 3: Server-Side Pagination

**Goal:** Return only the requested page of applications from the server, reducing serialization, network transfer, and client memory.

#### Proto Changes

Add pagination fields to `ApplicationQuery` in `server/application/application.proto`:

```protobuf
message ApplicationQuery {
    // ... existing fields 1-8 ...

    // Maximum number of applications to return in a single response. 0 means no limit (default).
    optional int64 limit = 9;

    // Continue token from a previous paginated response for stable cursor-based pagination.
    // The token is the qualified name (namespace/name) of the first application on the next page.
    // Mutually exclusive with offset -- continue takes precedence when both are provided.
    optional string continue = 10;

    // Offset for simple numeric pagination. Use continue for stable pagination under concurrent modifications.
    optional int64 offset = 11;
}
```

#### Why Both `continue` and `offset`

**`offset`/`limit`** is simpler for UIs that track page numbers. The UI can compute `offset = page * pageSize` directly.

**`continue` tokens** provide stable pagination when the underlying dataset changes between page requests.
If an application is added or deleted while a user is browsing, offset-based pagination can show duplicates or skip items. A continue token based on the last-seen application name resumes correctly regardless of modifications.

Both are supported. When both `continue` and `offset` are provided, `continue` takes precedence.

#### Server-Side Pagination Logic

After RBAC filtering and sorting in `server/application/application.go`:

```go
// Apply pagination if limit is specified.
totalCount := int64(len(newItems))
var continueToken string

if q.GetLimit() > 0 {
    limit := int(q.GetLimit())
    startIdx := 0

    if q.GetContinue() != "" {
        // Binary search for the continue token in the sorted list (O(log n)).
        startIdx = sort.Search(len(newItems), func(i int) bool {
            return newItems[i].QualifiedName() >= q.GetContinue()
        })
    } else if q.GetOffset() > 0 {
        startIdx = int(q.GetOffset())
    }

    if startIdx >= len(newItems) {
        newItems = nil
    } else {
        end := startIdx + limit
        if end > len(newItems) {
            end = len(newItems)
        }
        // Set continue token to the first item of the NEXT page.
        if end < len(newItems) {
            continueToken = newItems[end].QualifiedName()
        }
        newItems = newItems[startIdx:end]
    }
}

// Standard Kubernetes ListMeta fields -- no custom response type needed.
listMeta := metav1.ListMeta{ResourceVersion: resourceVersion}
if q.GetLimit() > 0 {
    remainingCount := totalCount - int64(len(newItems))
    listMeta.Continue = continueToken
    listMeta.RemainingItemCount = &remainingCount
}

appList := v1alpha1.ApplicationList{
    ListMeta: listMeta,
    Items:    newItems,
}
```

**Key properties:**
- Continue token is the `QualifiedName()` of the first item on the next page (not base64 encoded -- simple and debuggable)
- Binary search (`sort.Search`) finds the resume position in O(log n)
- Response uses standard Kubernetes `ListMeta.Continue` and `ListMeta.RemainingItemCount` fields
- When `limit=0` or absent, all items are returned with no pagination metadata (backward compatible)

#### Application Stats in Response

With pagination, the UI cannot compute aggregate statistics (health/sync breakdowns, total count) from the current page alone. The proposal adds an `ApplicationListStats` field to the response containing aggregate counts over the full RBAC-filtered dataset:

```go
type ApplicationListStats struct {
    Total                int64                             `json:"total" protobuf:"bytes,1,opt,name=total"`
    TotalBySyncStatus    map[SyncStatusCode]int64          `json:"totalBySyncStatus,omitempty" protobuf:"bytes,2,opt,name=totalBySyncStatus"`
    TotalByHealthStatus  map[health.HealthStatusCode]int64 `json:"totalByHealthStatus,omitempty" protobuf:"bytes,3,opt,name=totalByHealthStatus"`
    AutoSyncEnabledCount int64                             `json:"autoSyncEnabledCount" protobuf:"bytes,4,opt,name=autoSyncEnabledCount"`
    Destinations         []ApplicationDestination          `json:"destinations" protobuf:"bytes,5,opt,name=destinations"`
    Namespaces           []string                          `json:"namespaces" protobuf:"bytes,6,opt,name=namespaces"`
    Labels               []ApplicationLabelStats           `json:"labels,omitempty" protobuf:"bytes,7,opt,name=labels"`
}
```

These stats are computed from the RBAC-filtered name list (cached via Phase 2) by iterating application objects from the informer without deep-copying.
The stats are also cacheable with a 10-second TTL.

#### Pagination Walk-Through

```
Request 1: GET /api/v1/applications?limit=50
  Server: informer → RBAC filter (or cache hit) → sort → slice [0:50]
  Response: {
    items: [50 apps],
    metadata: {
      continue: "ns/app-051",
      remainingItemCount: 950,
      resourceVersion: "12345"
    }
  }

Request 2: GET /api/v1/applications?limit=50&continue=ns/app-051
  Server: cache hit → sort → binary search for "ns/app-051" → slice [50:100]
  Response: {
    items: [50 apps],
    metadata: {
      continue: "ns/app-101",
      remainingItemCount: 900
    }
  }

UI page navigation (offset-based):
  GET /api/v1/applications?limit=50&offset=100
  Server: cache hit → slice [100:150]
  Response: {
    items: [50 apps],
    metadata: {
      continue: "ns/app-151",
      remainingItemCount: 850
    }
  }
```

#### UI Integration

**`ui/src/app/shared/services/applications-service.ts`** -- Add `limit` and `offset` to `QueryOptions`:

```typescript
interface QueryOptions {
    fields: string[];
    exclude?: boolean;
    selector?: string;
    appNamespace?: string;
    limit?: number;
    offset?: number;
}
```

**`ui/src/app/shared/components/paginate/paginate.tsx`** -- Add `totalCount` prop for server-side pagination mode:

```typescript
export interface PaginateProps<T> {
    // ... existing props ...
    // When provided, enables server-side pagination mode.
    // The data array is assumed to already be the correct page and is not sliced client-side.
    totalCount?: number;
}
```

When `totalCount` is provided:
1. Page count uses `totalCount` instead of `data.length`
2. Client-side `data.slice()` is skipped -- data is already the correct page from the server

**`ui/src/app/applications/components/applications-list/applications-list.tsx`** -- Key changes:

1. `loadApplications()` accepts `limit`/`offset` params, returns `Observable<{applications, totalCount}>`
2. `totalCount` is derived from `items.length + remainingItemCount` from the response metadata
3. DataLoader input includes `page` and `pageSize` so page changes trigger API reloads
4. Page size "all" (`-1`) sends no limit, falling back to full list behavior

### Use Cases

#### Use Case 1: Large-scale multi-tenant environment
As a platform engineer managing 2000+ applications across 50 namespaces,
I want the applications list to load in under 2 seconds
so that I can quickly find and inspect applications without waiting for the UI to become responsive.

#### Use Case 2: Page-by-page browsing
As a user, I want to navigate through the list of applications using pagination controls, with each page loading quickly from the server,
so that I do not need to download all 2000 applications to view page 3.

#### Use Case 3: CLI batch operations
As a CI/CD operator, I want `argocd app list` to fetch applications in batches of 500,
so that the CLI does not time out or consume excessive memory when listing thousands of applications.

#### Use Case 4: Namespace-scoped listing
As a tenant in a multi-tenant Argo CD installation, I want to filter applications by my namespace (app namespace, not destination namespace),
so that both the initial load and real-time watch events only include my team's applications.

#### Use Case 5: Status bar accuracy with pagination
As a user viewing a paginated application list, I want the health/sync status bar to show accurate totals across all my applications, not just the current page.

### Design Considerations

- **Why not Kubernetes native `limit`/`continue`?**
Argo CD has its own RBAC layer that operates above the Kubernetes API.
K8s native pagination returns items before RBAC filtering, so requesting `limit=50` might yield 12, 50, or 0 items after filtering -- making page sizes unpredictable.
The informer already holds all Application objects in memory, so direct API calls would add latency without reducing memory usage.

- **Why both `continue` and `offset`?**
`offset` is simple for UIs (`offset = page * pageSize`).
`continue` is resilient to concurrent modifications (if app-025 is deleted between page requests, offset=50 skips an item, but `continue=app-050` correctly resumes after app-050).
Both are supported for different use cases.

- **Why cache names instead of full objects?**
Caching the list of ~1000 qualified names (~50KB) is small and fast.
Full objects would be large, stale quickly, and require deep-copy anyway.
Names are used to do targeted informer lookups, which always return fresh data.

- **Why `resourceVersion` in cache keys?**
The informer's `LastSyncResourceVersion` changes on any Application mutation.
Using it in the cache key provides automatic invalidation without explicit cache-busting logic -- old entries just miss and expire via TTL.

### Implementation Details/Notes/Constraints

#### Component: API Server (Go)

**File: `server/application/application.proto`**
- Add `limit` (field 9), `continue` (field 10), `offset` (field 11) to `ApplicationQuery`
- Add `ApplicationListStats` type and optional `stats` field to `ApplicationList`

**File: `server/application/application.go`**
- Refactor `List()` to check RBAC name cache before running full RBAC loop
- Add pagination logic (binary search for continue token, offset slicing) after RBAC filtering
- Populate `ListMeta.Continue` and `ListMeta.RemainingItemCount` only when `limit > 0`
- Add helper methods: `listAppsWithRBACFilter()`, `fetchAppsByNames()`, `parseQualifiedName()`
- Fix Watch handler to scope initial ADDED events by `appNamespace`

**File: `server/cache/cache.go`**
- Add `AppListSummary` struct
- Add `GetAllowedAppNames()` / `SetAllowedAppNames()` with 15s TTL
- Add `GetAppListSummary()` / `SetAppListSummary()` with 10s TTL
- Cache key includes `resourceVersion` for natural invalidation

#### Component: Web UI (TypeScript/React)

**File: `ui/src/app/shared/services/applications-service.ts`**
- Add `limit` and `offset` to `QueryOptions` interface
- Include in query parameters when present

**File: `ui/src/app/shared/components/paginate/paginate.tsx`**
- Add `totalCount` prop for server-side pagination mode
- Skip `data.slice()` when `totalCount` is provided

**File: `ui/src/app/applications/components/applications-list/applications-list.tsx`**
- `loadApplications()` returns `Observable<{applications, totalCount}>`
- DataLoader input includes `page`/`pageSize` for reload triggers
- `ViewPref` loads full `ViewPreferences` to expose `pageSize`

**File: `ui/src/app/shared/services/view-preferences-service.ts`**
- Add `appNamespaceFilter` to `AppsListPreferences`

#### Component: CLI

- Update `argocd app list` to support `--offset` and `--limit` flags
- When no flags specified, use pagination to load all applications in batches (e.g., 500 per batch)
- Graceful fallback: if the server returns more items than `limit`, assume pagination is not supported (backward compatibility with older servers)

#### Component: Documentation

- Document new API parameters (`limit`, `continue`, `offset`) in API reference
- Update UI user guide with pagination behavior
- Update CLI reference with new flags
- Add performance tuning guide for large installations

### Security Considerations

- **No new attack surface.** Pagination parameters are simple integers and strings. The `continue` token is a qualified application name (not an opaque token encoding sensitive data).
- **RBAC enforcement unchanged.** All applications are still RBAC-filtered before pagination is applied. The cached name list only contains names the user is authorized to see.
- **Cache isolation.** RBAC name caches are keyed by username, ensuring User A cannot see User B's cached results.
- **Cache timing.** The 15-second TTL means RBAC policy changes take up to 15 seconds to take effect. This is acceptable for most environments. For stricter requirements, the TTL can be reduced or active invalidation can be added.

### Risks and Mitigations

#### Risk: Client-side filters show incorrect counts
**Description:** With pagination, client-side filters (health, sync status) only see the current page, not the full dataset. The status bar would show "3 Healthy" instead of "800 Healthy."
**Mitigation:** The `ApplicationListStats` field in the response provides aggregate counts computed over the full RBAC-filtered dataset. The UI uses these stats for the status bar.

#### Risk: Cache staleness after RBAC policy changes
**Description:** If RBAC policies are updated in `argocd-rbac-cm`, the cached allowed-names list may be stale for up to 15 seconds.
**Mitigation:** The 15-second TTL provides eventual consistency. For environments requiring immediate RBAC enforcement, the TTL can be reduced or an RBAC policy change handler can invalidate the cache.

#### Risk: Continue token instability under RBAC changes
**Description:** If RBAC permissions change between page requests, the result set may shift -- an application visible on page 1 might not be visible on page 2.
**Mitigation:** This is a fundamental property of any paginated system where the underlying dataset can change. The continue token still resumes at the correct position in the sorted list. Offset-based pagination has the same limitation. The 15-second cache TTL limits the window of inconsistency.

### Upgrade / Downgrade Strategy

- **Feature is opt-in via client behavior.**
The server changes are backward compatible. Existing clients that do not send `limit`/`offset`/`continue` parameters receive the full application list unchanged.
- **No configuration required.**
Server-side caching activates automatically but only affects requests that include pagination parameters.
- **CLI backward compatibility.**
If the CLI sends `limit` but the server returns more items than `limit`, the CLI should assume pagination is not supported and treat the response as a full list.
This allows CLI upgrades ahead of server upgrades.
- **UI backward compatibility.**
If the server response does not include `RemainingItemCount`, the UI falls back to client-side pagination (existing behavior).
This allows UI upgrades ahead of server upgrades.
- **Downgrade path.**
Remove `limit`/`offset` from API calls to revert to full list behavior. No data migration or state cleanup needed.

### Backward Compatibility

| Scenario | Behavior |
|----------|----------|
| CLI `argocd app list` (no flags) | No limit sent. All apps returned, no pagination metadata |
| Existing UI without changes | No limit sent. All apps returned (DataLoader defaults) |
| API clients not sending limit | Full list returned, identical to pre-change behavior |
| Page size "all" (-1) in UI | `effectivePageSize=0`. No limit. All apps loaded |
| Watch endpoint | Unaffected by pagination. Streams all events regardless of limit |
| Older server + newer CLI with `--limit` | Server ignores unknown params, returns full list. CLI detects and handles gracefully |

## Implementation Order

| Phase | Effort | Impact | Dependencies |
|-------|--------|--------|-------------|
| 1: Namespace filtering + Watch fix | 1-2 days | High (multi-tenant) | None |
| 2: RBAC name caching | 2-3 days | High (reduces per-page RBAC cost) | None |
| 3: Server-side pagination | 3-5 days | High (all users) | Phase 2 recommended |

**Why Phase 2 before Phase 3:**
Pagination without caching still evaluates RBAC on all 1000+ applications for every page request.
With the cached allowed-names list, page 2/3/4 requests become a simple slice of the cached list + targeted informer lookups -- no RBAC loop.

## Drawbacks

* **Lack of profiling data**: We currently lack full end-to-end profiling data about the performance of the list UI to confirm how much pagination would improve performance
* **Inability to use k8s pagination**: our UI offers features like sorting by app name, which Kubernetes does not support; so Argo will need to load the full list from Kubernetes regardless of Argo-side pagination
* **Complex Implementation**: a [couple](https://github.com/argoproj/argo-cd/pull/22444) [attempts](https://github.com/argoproj/argo-cd/pull/25097) to implement the feature have failed due to high complexity and unexpected edge cases

- **Cache staleness window.**
RBAC-filtered name caches have a 15-second TTL. During this window, RBAC policy changes are not reflected. This is a tradeoff between performance and consistency.
- **Increased API server complexity.**
The List handler gains cache-check logic, pagination slicing, and stats computation. However, the code is well-factored into helper methods.
- **Sort order limitation.**
Server-side pagination sorts by qualified name (deterministic and efficient).
Client-side sort options (Created At, Last Sync) only reorder within the current page.
Server-side sort by arbitrary fields would require additional work.

## Alternatives

* **Improve frontend performance**:
  * **Upgrade to React 19**: [will provide](https://github.com/argoproj/argo-cd/pull/27091) immediate performance benefits and unlock new profiling tooling to investigate bottlenecks
  * **Improve RBAC evaluation**: one of the slow parts of listing apps is evaluating RBAC for each app, and we can improve that by doing things like [caching compiled glob patterns](https://github.com/argoproj/argo-cd/pull/25759)
* **Reduce list payload**: some data sent to the UI is unnecessary and [can be eliminated](https://github.com/argoproj/argo-cd/pull/25451) relatively easily

### Alternative 1: Server-side filtering without pagination

Move all filter logic (health, sync, cluster, namespace, search) to the server but return the full filtered list without pagination.
This reduces payload when filters are active but does not help the unfiltered default view.

**Decision:**
Complementary to pagination, not a replacement. Should be implemented as a follow-up.

### Alternative 2: `minName`/`maxName` cursor approach (original proposal)

The original proposal suggested `minName`/`maxName` fields for cursor-based pagination.
This is functionally equivalent to `continue`/`limit` but uses a different API shape.

**Decision:**
`limit`/`continue` follows the Kubernetes API convention (`ListOptions`), making it more familiar to users.
`offset`/`limit` additionally provides simple numeric pagination for the UI.
Both cursor and offset are supported in the current design.

### Alternative 3: Separate stats endpoint

Instead of including `ApplicationListStats` in the List response, expose a separate `GET /api/v1/applications/stats` endpoint.

**Decision:**
A separate endpoint requires an additional HTTP request per page load.
Including stats in the List response (computed from the cached name list) is more efficient.
However, a separate stats endpoint could be added later for clients that need stats without fetching a page.

### Alternative 4: GraphQL or cursor-based relay pagination

Implement a GraphQL endpoint with Relay-style cursor pagination for the applications list.

**Decision:**
Adds significant complexity and a new dependency.
The gRPC+REST API pattern is well-established in Argo CD. Simple `limit`/`offset`/`continue` achieves the same result with minimal changes.

## Related Issues

- https://github.com/argoproj/argo-cd/issues/12707 -- Applications list API should support pagination
- https://github.com/argoproj/argo-cd/issues/14849 -- UI performance with large number of applications

## References

- [Kubernetes API Concepts: Retrieving large results sets in chunks](https://kubernetes.io/docs/reference/using-api/api-concepts/#retrieving-large-results-sets-in-chunks)
- [Rancher Steve - API pagination](https://github.com/rancher/steve)
