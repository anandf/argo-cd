---
title: Separate API Types and Generated Clients Into Their Own Go Module
authors:
  - "@anandf"
sponsors:
  - TBD
reviewers:
  - TBD
approvers:
  - TBD

creation-date: 2026-11-19
last-updated: 2026-11-23
---

# Separate API Types and Generated Clients Into Their Own Go Module

Related issues:

* [#14727 - Separate out client and type files from rest of project](https://github.com/argoproj/argo-cd/issues/14727)
* [#4055 - ArgoCD go.mod requires k8s.io/kubernetes directly, resulting in go build unknown revision v0.0.0](https://github.com/argoproj/argo-cd/issues/4055)

## Open Questions

1. **Module path.** Is `github.com/argoproj/argo-cd/api/v3` rooted at `./api` the right choice,
   matching the precedent already set by `github.com/argoproj/argo-cd/gitops-engine/v3` at
   `./gitops-engine`? Issue #14727 originally suggested Go workspaces. The repository has since
   adopted the nested-module-plus-`replace` pattern, which appears to be the better fit and makes
   no difference to what consumers see.
2. **Release timing.** Ship the compatibility shims in a v3 minor and remove the old import paths
   in v4, or land the whole change in v4?
3. **Independent tagging.** Should the API module be tagged `api/vX.Y.Z` the way
   `gitops-engine/vX.Y.Z` is tagged, so that consumers can pin it independently of Argo CD releases?
4. **Scope of the shims.** The implementation generates shims for the moved utility packages
   (`util/env`, `util/glob`, and the rest) as well as for the API packages, because removing them
   from the root module would otherwise break anyone importing them. Should those utility shims
   ship, or should those packages be treated as internal and allowed to break?
5. **Downstream impact of the two breaking changes.** Do any of the known downstream projects call
   `Repository.GetGitCreds`/`GetHelmCreds`/`GetOCICreds`, or pass Argo CD's health and sync enum
   values directly into gitops-engine APIs? See [Risks and Mitigations](#risks-and-mitigations).

## Summary

Argo CD's main module has a dependency graph sized for the application controller, repo-server,
API server and CLI, and it depends on the separately tagged `gitops-engine` module, so consumers
receive both. Neither module contains only the CRD types, so projects that need nothing else
inherit all of it.

This proposal moves `pkg/apis/...` and `pkg/client/...` into a new nested Go module,
`github.com/argoproj/argo-cd/api/v3`, so that downstream projects can depend on the Argo CD API
surface alone. The root module keeps the existing import paths working for one release cycle via
generated type-alias shims.

> [!NOTE]
> This proposal has been implemented in full across four phases. The figures below are measured
> against the implementation, not estimated. See [Implementation phases](#implementation-phases)
> for what each phase did and [Validation](#validation) for how it was verified.

## Motivation

Several projects depend on Argo CD purely for its API surface:

* `github.com/redhat-developer/gitops-operator`
* `github.com/argoproj-labs/argocd-image-updater`
* `github.com/argoproj-labs/argocd-agent`

They use the `Application`, `AppProject` and `ApplicationSet` types and the generated clientset to
create and manage child objects, plus a small number of utility methods.

Today a consumer importing only those two packages cannot even resolve its module graph:

```text
$ go mod tidy
k8s.io/kubernetes/pkg/apis/core/validation imports
    k8s.io/kubelet/pkg/apis: reading k8s.io/kubelet/go.mod at revision v0.0.0:
    unknown revision v0.0.0
```

This is the failure reported in #4055, open since 2020. The only workaround is for every consumer
to copy Argo CD's roughly 35 `k8s.io/*` staging `replace` directives into their own `go.mod`, which
transitively pins their Kubernetes library versions to Argo CD's. That pinning is the core complaint
in #14727.

### Goals

* A downstream project can depend on the Argo CD CRD types and generated clientset without
  requiring any `replace` directives in its own `go.mod`.
* `k8s.io/kubernetes` is not reachable from the API module's dependency graph.
* Existing consumers continue to compile without source changes for at least one release cycle.
* The root module, its binaries and its behaviour are unchanged.
* Measurable result, verified against the implementation:

  | Metric (consumer importing `Application` + typed clientset) | Before | After |
  | --- | --- | --- |
  | `k8s.io/*` staging `replace` directives required | ~35 (mandatory) | 0 |
  | Modules in build list | 455 | 195 |
  | Non-stdlib packages compiled | 1084 | 343 |
  | gitops-engine packages reachable | 14 | 0 |
  | `k8s.io/kubernetes` packages reachable | 98 | 0 |

  Distinct external dependency roots reachable from `pkg/apis/application/v1alpha1` drop from 205
  to 79.

### Non-Goals

* Moving `pkg/apiclient` (the gRPC API client). It is heavy by nature and is not what these
  consumers need. It stays in the root module.
* Changing any CRD schema, wire format, generated protobuf, or runtime behaviour.
* Changing gitops-engine in any way. It is deliberately left untouched; see
  [Alternatives](#alternatives) for the leaf-package approach that was considered and rejected.
* Removing `k8s.io/kubernetes` from the root module, which would let Argo CD drop its own
  `k8s.io/*` replace directives. That is a separate and larger problem; see Alternative 5.
* Reducing the root module's own dependency graph. The root module still needs everything it needs
  today; this proposal only changes what *consumers of the API* must pull in.

## Proposal

### Use cases

#### Use case 1

As a maintainer of `argocd-image-updater`, I want to import `v1alpha1.Application` and the
generated clientset without having `k8s.io/kubernetes` in my dependency graph, so that `go mod tidy`
succeeds without copying Argo CD's `replace` block.

#### Use case 2

As a maintainer of `gitops-operator`, I want to choose my own `client-go` version independently of
the version Argo CD pins, so that I can align with the controller-runtime version my operator uses.

#### Use case 3

As a maintainer of an existing downstream project, I want my current code to keep compiling when I
bump Argo CD, so that I can migrate to the new import path on my own schedule.

### Implementation Details/Notes/Constraints

#### What coupled the types to everything else

The coupling was narrow and concentrated, which is what made the split tractable. Five places
accounted for nearly all of it.

1. **gitops-engine `pkg/health` and `pkg/sync/common`** — the largest contributor. The CRD types
   used these for string enum types only (`HealthStatusCode`, `SyncPhase`, `OperationPhase`,
   `ResultCode`, `HookType`) plus `kube.ResourceKey`, but those enums appear as struct field types
   on `ApplicationStatus`, `ResourceResult` and `HealthStatus`, so they are part of the CRD public
   surface and also appear in `generated.pb.go`.

   Both packages import `pkg/utils/kube`, whose `scheme` subpackage imports 24
   `k8s.io/kubernetes/pkg/apis/*/install` packages to register the legacy scheme. That subpackage
   is the **single** entry point for `k8s.io/kubernetes` in the entire graph. This is the direct
   cause of #4055: a consumer importing only the CRD types and the clientset still reached 14
   gitops-engine packages, `scheme` and `pkg/diff` among them.
2. **Credential constructors on `Repository`** — `GetGitCreds`, `GetHelmCreds`, `GetOCICreds` and
   `getCAPath` in `repository_types.go`, about 105 lines in total, were the only reason the types
   reached `util/git`, `util/helm`, `util/oci`, `util/cert` and `util/workloadidentity`.
3. **`util/rbac`** — eight string constants used by `AppProject` role validation (`ActionGet`,
   `ActionCreate`, …, `ProjectScoped`) pulled in `casbin`.
4. **`common`** — three string constants (`DefaultRepoType`, `AnnotationKeyLinkPrefix`,
   `AnnotationApplicationSetRefresh`) pulled in `go-redis`, `client-go` and `grpc`.
5. **`git.NormalizeGitURL`, `helm.IsHelmOciRepo` and the `util/http` transport wrappers** — pure
   helpers that happened to live in heavy packages.

#### Module layout

Follows the precedent already established in this repository by `gitops-engine`.

| Module | Directory | Contents |
| --- | --- | --- |
| `github.com/argoproj/argo-cd/v3` | `.` | unchanged |
| `github.com/argoproj/argo-cd/gitops-engine/v3` | `./gitops-engine` | unchanged |
| `github.com/argoproj/argo-cd/api/v3` | `./api` | **new** — `pkg/apis/...`, `pkg/client/...`, and the leaf utilities the types need |

The new module is wired into the root module with a `replace` directive exactly as `gitops-engine`
is today:

```go
replace github.com/argoproj/argo-cd/api/v3 => ./api
```

> [!NOTE]
> A nested module's import path must match its directory, so the import path necessarily changes
> from `github.com/argoproj/argo-cd/v3/pkg/apis/...` to
> `github.com/argoproj/argo-cd/api/v3/pkg/apis/...`. There is no way for a submodule to keep a path
> owned by the root module. This is why the compatibility shims exist.

#### The API module defines its own enum types

The API module declares the enums it needs natively, rather than importing them from
gitops-engine:

```go
// api/pkg/apis/application/v1alpha1
type HealthStatusCode string
type OperationPhase string
type ResultCode string
type SyncPhase string
type HookType string
type ResourceKey struct{ Group, Kind, Namespace, Name string }
```

**gitops-engine is left completely untouched**, and the API module takes no dependency on it. This
is what makes the module genuinely standalone: a downstream project importing the CRD types and the
clientset pulls in no gitops-engine packages at all, which is the entire point of the exercise.

These are all `string`-based named types carrying the same values, so the wire format, the CRD
schema and the generated protobuf are unchanged. Only the Go type identity changes.

> [!WARNING]
> Argo CD's own code passes these values into gitops-engine APIs, and those call sites now need an
> explicit conversion, for example `health.HealthStatusCode(app.Status.Health.Status)`. In practice
> the conversions clustered at three boundaries — `controller/sync.go`, `controller/health.go` and
> `controller/cache/cache.go` — rather than being spread across the code base.

`ResourceKey` is a lookup key rather than API surface: it is never serialized and appears in no CRD
field. It must therefore be excluded from the generators, or `go-to-protobuf` treats it as a CRD
message and emits a competing `String()` method:

```go
// +k8s:deepcopy-gen=false
// +k8s:openapi-gen=false
// +protobuf=false
type ResourceKey struct{ ... }
```

#### Decoupling the credential constructors

The credential constructors moved off `Repository` into a new `util/repocreds` package in the root
module, which owns the dependency on `util/git`, `util/helm`, `util/oci`, `util/cert` and
`util/workloadidentity`:

```go
// before
creds := repo.GetGitCreds(store)

// after
creds := repocreds.GetGitCreds(repo, store)
```

22 call sites, across `reposerver` and `commitserver`.

#### Leaf packages

Four constants-and-helpers packages were split out so the types could stop importing their heavy
parents. In every case the original package re-exports the moved symbols, so no existing caller
changed:

| New package | Contents | Severs dependency on |
| --- | --- | --- |
| `util/git/giturl` | 10 URL helpers including `NormalizeGitURL` | go-git transport stack, go-github, Azure SDK |
| `util/helm/helmurl` | `IsHelmOciRepo` | the Helm client |
| `util/rbac/actions` | 21 resource/action constants and `ProjectScoped` | casbin |
| `util/kubetransport` | `WithRetry`, `WithServerSideTimeout` and their env vars | `common`, and through it go-redis and grpc |

The three `common` constants the types need were declared in
`pkg/apis/application/v1alpha1/constants.go`, with `common` re-exporting them so there remains a
single source of truth.

#### Backward compatibility shims

`hack/gen-api-aliases` walks the API module and generates a `zz_generated_aliases.go` shim at each
package's original path in the root module. It runs as part of `make codegen`, and standalone as
`make api-aliases`.

```go
// Code generated by hack/gen-api-aliases. DO NOT EDIT.
//
// Deprecated: this package moved to github.com/argoproj/argo-cd/api/v3/pkg/apis/application/v1alpha1.
package v1alpha1

import api "github.com/argoproj/argo-cd/api/v3/pkg/apis/application/v1alpha1"

type (
	Application = api.Application
	AppProject  = api.AppProject
	// ...
)
```

22 shim packages are generated, 375 aliases for `v1alpha1` alone. Because these are **type
aliases** rather than new named types, the methods defined on those types are reachable through the
shim automatically, and type identity is preserved: a `*v1alpha1.Application` constructed through
the old path satisfies a signature expecting the new type, and can be passed between the two
freely.

> [!IMPORTANT]
> The shims keep the old paths *compiling*, but they live in the root module. A consumer importing
> them still depends on `github.com/argoproj/argo-cd/v3` and therefore still needs the `k8s.io/*`
> replace directives. The dependency reduction only arrives once a consumer imports the API module
> directly. The shims buy a migration window, not a free upgrade.

Functions and variables are re-exported as `var X = api.X`. For functions this is transparent. For
variables it copies the value at init, which is fine for the read-only values in these packages but
means a consumer mutating one through a shim would not affect the API module's copy.

#### Dependency pinning: `require`, not `replace`

> [!WARNING]
> A `replace` directive in a dependency is **ignored** by the module that consumes it. Pinning the
> API module's Kubernetes versions with `replace` worked locally but left the first real consumer
> resolving `k8s.io/apimachinery` v0.37.2 against `sigs.k8s.io/structured-merge-diff` v7, which
> does not compile.

The API module therefore expresses its constraints with `require`:

* `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go`, `k8s.io/apiextensions-apiserver` at
  `v0.37.0`
* `k8s.io/kube-openapi` at the version those require, which uses structured-merge-diff v6
* `sigs.k8s.io/kustomize/api` at `v0.21.1`, because `v0.21.3` requires a newer `kube-openapi` that
  pulls structured-merge-diff v7

The root module avoids this trap only because module graph pruning keeps it from walking
`kustomize`'s requirements. Care is needed not to let `go mod tidy` float these.

#### Codegen

The generators derive generated import paths from the main module, so the API-related ones must run
with the API module as the main module. The following were updated:

| File | Change |
| --- | --- |
| `hack/update-codegen.sh` | `cd`s into `api/` before running `gen_helpers`/`gen_client`; without this it emitted `argo-cd/v3/api/...` paths |
| `hack/generate-proto.sh` | package list points at the API module; adds an `api/v3` symlink so GOPATH-based resolution works |
| `hack/update-openapi.sh` | output package and report path point at the API module |
| `hack/gen-crd-spec/main.go` | `controller-gen` paths point at `./api/pkg/apis/application/...` |
| `.mockery.yaml` | clientset interface package path updated |
| Root `*.proto` files | both the `import` lines **and** the fully-qualified type references (`github.com.argoproj.argo_cd.api.v3....`) |
| `Makefile` | adds `test-api` and `api-aliases` targets; extends the `TEST_MODULE` filters, mirroring `test-gitops-engine` |

> [!WARNING]
> `gen_client` clears its output directory before regenerating. A codegen run that fails part-way
> therefore leaves `api/pkg/client` empty. This is worth knowing before debugging a failed CI
> codegen job.

Running `make codegen` by hand was essential rather than optional: the embedded gzipped
`FileDescriptorProto` in `generated.pb.go` carried five gitops-engine references in its gogo
`casttype` annotations that a source-level edit does not touch. Only regeneration removed them.

## Implementation phases

The work splits into four phases. Each is independently mergeable, and the first two change no
import paths at all.

> [!NOTE]
> The order matters. It is tempting to create the new module first and move the types into it as
> step one, but that would simply relocate the problem: the types still dragged gitops-engine,
> casbin, oras-go and the Azure SDK behind them, so the new module would have been just as heavy
> as the old one. Phases 1 and 2 make the types light *in place*; only then is moving them
> worthwhile. Phases 1 and 2 also change no import paths, so they can merge early and independently
> without colliding with other work.

### Phase 1 — Declare the enum types natively

**Goal.** Remove `k8s.io/kubernetes` from the API package's dependency graph, which is the root
cause of #4055.

**Objective.** Stop borrowing the health and sync vocabulary from gitops-engine. Declare it in
`pkg/apis/application/v1alpha1` instead, and convert explicitly wherever Argo CD hands these values
to the sync engine. gitops-engine is not touched and no import paths change.

**Steps.**

1. Add `health_types.go` to `pkg/apis/application/v1alpha1` declaring `HealthStatusCode` and its
   six constants, mirroring `gitops-engine/pkg/health` exactly.
2. Add `sync_types.go` declaring `OperationPhase`, `ResultCode`, `SyncPhase` and `HookType`, their
   constants, the four `OperationPhase` predicates (`Completed`, `Running`, `Successful`, `Failed`)
   and `NewHookType`, mirroring `gitops-engine/pkg/sync/common`.
3. Add `resource_key.go` declaring `ResourceKey`. Mark it `+k8s:deepcopy-gen=false`,
   `+k8s:openapi-gen=false` and `+protobuf=false`: it is a lookup key, never serialized, and
   without the markers `go-to-protobuf` treats it as a CRD message and emits a competing `String()`
   method.
4. Switch the field declarations in `types.go` from the gitops-engine types to the new local ones
   (`ApplicationStatus.Health`, `OperationState.Phase`, the four fields on `ResourceResult`, and
   the `FindResource` signature), and drop the now-unused gitops-engine imports.
5. Build the repository and work through the compile errors. Each one is a boundary where an Argo
   CD value meets a gitops-engine API. Classify each file before changing it:
     * **CRD-side** (stores values on an Application): use the new `v1alpha1` constants.
     * **Engine-side** (implements a gitops-engine interface, such as `controller/hook.go` and
       `util/lua/lua.go`): leave on the gitops-engine types.
6. Add explicit conversions at the three genuine boundaries — `controller/sync.go` (both
   directions), `controller/health.go` (convert once on entry, then work in the API type) and
   `controller/cache/cache.go`.
7. Update the 26 e2e test files that dot-imported `gitops-engine/pkg/sync/common` for enums now
   provided by `v1alpha1`, taking care not to leave a package imported twice.
8. Add `test/apicompat` as a guard that the two constant sets cannot drift. Place it in the root
   module, not in the API package: inside the API package it would add gitops-engine to that
   module's test dependencies and defeat the purpose.
9. Run `make codegen` and commit the result. This is not optional — the embedded descriptor in
   `generated.pb.go` carries gitops-engine references in its gogo `casttype` annotations that no
   source edit touches.

**Result.** `k8s.io/kubernetes` 98 packages → **0**. gitops-engine 14 → 3. Dependency roots
205 → 156.

### Phase 2 — Decouple the remaining utilities

**Goal.** Remove the remaining heavyweight third-party libraries — casbin, oras-go, the Azure SDK,
go-redis, go-github — from the API package's graph.

**Objective.** Unbolt the machinery still attached to the types: the credential constructors, and
the handful of constants and helpers that each drag an entire subsystem behind them. Still a single
module; no import paths change.

**Steps.**

1. Create `util/repocreds` in the root module. Move `GetGitCreds`, `GetHelmCreds`, `GetOCICreds`
   and the shared `getCAPath` out of `repository_types.go`, converting the methods to free
   functions that take `*v1alpha1.Repository`. This package, not the API types, now owns the
   dependency on `util/git`, `util/helm`, `util/oci`, `util/cert` and `util/workloadidentity`.
2. Update the 22 call sites in `reposerver` and `commitserver` from `repo.GetGitCreds(store)` to
   `repocreds.GetGitCreds(repo, store)`.
3. Move the credential tests out of the API package into `util/repocreds`, adjusting the fixture
   paths, which are relative to the package directory.
4. Create `util/git/giturl` holding the ten pure URL helpers (`NormalizeGitURL`, `IsSSHURL`,
   `SanitizeRepoURL` and the rest). Have `util/git` re-export all of them so existing callers are
   untouched, and move their tests across too.
5. Create `util/helm/helmurl` holding `IsHelmOciRepo`, re-exported from `util/helm`.
6. Create `util/rbac/actions` holding the 21 resource and action constants plus `ProjectScoped`,
   re-exported from `util/rbac`. The casbin enforcer stays where it is.
7. Create `util/kubetransport` holding `WithRetry`, `WithServerSideTimeout`, their two environment
   variable names and the two transport types. These move out of `util/http` entirely, since the
   API types were their only consumer.
8. Add `pkg/apis/application/v1alpha1/constants.go` with the three constants the types need from
   `common`, and have `common` re-export them so there is a single source of truth.
9. Repoint the API package's imports at the new leaf packages and delete the heavy ones.
10. Build, vet, test and lint both the API package and the repository. Watch for test-only import
    cycles: making `common` re-export from `v1alpha1` creates one if an internal test in
    `v1alpha1` imports `common`.

**Result.** Dependency roots 156 → **80**. gitops-engine 3 → **0**. casbin, oras-go, Azure SDK,
go-redis, go-github and ghinstallation all gone; only go-git remains, via `giturl`.

### Phase 3 — Create the module

**Goal.** Give downstream projects a module they can depend on with no `replace` directives.

**Objective.** Move the now-light packages into their own module and re-wire the build, code
generation and test tooling for a two-module repository.

**Steps.**

1. Create the `api/` directory and `api/go.mod` declaring module
   `github.com/argoproj/argo-cd/api/v3`.
2. Move `pkg/apis` and `pkg/client` to `api/pkg/apis` and `api/pkg/client`. Use `git mv` for
   tracked files; plain `mv` for anything not yet committed.
3. Move the nine leaf utility packages the types need — `util/env`, `util/glob`, `util/hash`,
   `util/regex`, `util/security`, `util/git/giturl`, `util/helm/helmurl`, `util/kubetransport` and
   `util/rbac/actions` — preserving their relative paths under `api/`. Confirm first that this set
   is closed, that is, that nothing in it depends on a package staying behind.
4. Rewrite every import of a moved package across the repository, from
   `github.com/argoproj/argo-cd/v3/<path>` to `github.com/argoproj/argo-cd/api/v3/<path>`. This
   touches 473 files and is purely mechanical.
5. Add the `require` and `replace github.com/argoproj/argo-cd/api/v3 => ./api` entries to the root
   `go.mod`, mirroring how `gitops-engine` is wired.
6. Pin the API module's Kubernetes versions in `api/go.mod` using **`require`**, not `replace` — a
   dependency's `replace` is ignored by whoever consumes it. Pin `k8s.io/api`,
   `k8s.io/apimachinery`, `k8s.io/client-go` and `k8s.io/apiextensions-apiserver` to `v0.37.0`, and
   `sigs.k8s.io/kustomize/api` to `v0.21.1` so it does not pull a `kube-openapi` that needs
   structured-merge-diff v7.
7. Remove any dependency the API module's tests have on the root module. One test needed its RSA
   fixture inlined rather than imported from `util/test`.
8. Run `go mod tidy` in both modules and confirm neither floats the pinned versions back.
9. Update the code generation scripts so the API generators run with the API module as the main
   module:
     * `hack/update-codegen.sh` — `cd` into `api/` before `gen_helpers`/`gen_client`, otherwise the
       generated import paths come out as `argo-cd/v3/api/...`.
     * `hack/generate-proto.sh` — point the package list at the API module and add an `api/v3`
       symlink so GOPATH-based resolution works.
     * `hack/update-openapi.sh` — update the output package and report path.
     * `hack/gen-crd-spec/main.go` — point `controller-gen` at `./api/pkg/apis/application/...`.
     * `.mockery.yaml` — update the clientset interface package path.
10. Update the root `.proto` files in **two** places each: the `import` lines, and the
    fully-qualified type references, which embed the module path as
    `github.com.argoproj.argo_cd.api.v3....`.
11. Add `test-api` to the `Makefile` and extend the `TEST_MODULE` filters, mirroring
    `test-gitops-engine`.
12. Run `golangci-lint fmt` across both modules. The new paths belong in the third-party import
    group, since `.golangci.yaml` lists only `argo-cd/v3` as a local prefix, exactly as
    `gitops-engine` is treated.
13. Verify by creating a throwaway consumer project that requires only the API module with no
    `replace` directives, and confirming it tidies, builds and runs.

**Result.** A consumer importing only the API module builds and runs with **zero** `replace`
directives: 195 modules in the build list, 343 non-stdlib packages, no gitops-engine, no
`k8s.io/kubernetes`.

### Phase 4 — Compatibility shims

**Goal.** Give existing consumers a migration window rather than a hard break.

**Objective.** Leave a forwarding address at every import path that moved, generated from the real
thing so it cannot drift.

**Steps.**

1. Write `hack/gen-api-aliases`. It walks the packages under `api/`, parses each one's exported
   declarations, and writes a `zz_generated_aliases.go` at the same relative path in the root
   module. Types are emitted as `type X = api.X`; constants as `const X = api.X`; functions and
   variables as `var X = api.X`.
2. Run it and build. It generates 22 shim packages, 375 aliases for `v1alpha1` alone.
3. Add a `make api-aliases` target and wire it into `codegen-local` and `codegen-local-fast`, after
   `mockgen`, so the shims are always regenerated alongside the code they mirror.
4. Write `docs/developer-guide/api-module.md` covering the new import paths, the find-and-replace
   migration, and the two changes aliases cannot carry. Add it to the mkdocs navigation.
5. Verify with a throwaway consumer written entirely against the **old** import paths: it must
   compile unchanged, call methods on aliased types, and pass a shim-typed value into a function
   expecting the API module's type.

**Result.** A consumer written against the old import paths compiles and runs unchanged, including
methods on aliased types and passing those types into functions expecting the API module's types.

## Validation

* `make codegen-local` exits 0, and a second consecutive run changes no files.
* Both modules build and vet cleanly; `golangci-lint` reports 0 issues in each.
* The API module's tests pass. In the root module, the only failures are four pre-existing
  GPG-dependent tests, confirmed to fail identically at `HEAD` in a clean worktree.
* A consumer importing `github.com/argoproj/argo-cd/api/v3` builds **and runs** with no `replace`
  directives.
* A consumer written against the old import paths builds **and runs** unchanged against the shims,
  and passes a shim-typed `*Application` into a function expecting the API module's type, proving
  type identity holds across the boundary.
* `generated.proto` is byte-identical to its pre-change form, confirming the wire schema and CRDs
  did not change.

### Security Considerations

* The change is a relocation of existing code. No CRD schema, validation logic, authentication path
  or RBAC enforcement behaviour changes.
* The `rbac` action constants and `ProjectScoped` map moved to a constants-only leaf package. The
  casbin enforcer and all policy evaluation logic stay in `util/rbac` in the root module. Only the
  constant values moved; no enforcement code is duplicated.
* Credential construction moved from methods on `Repository` to free functions in `util/repocreds`.
  The logic was relocated verbatim, so credential selection precedence is unchanged. This should be
  reviewed carefully, since the ordering of the checks in `GetGitCreds` is security-relevant.
* Reducing the API module's dependency surface is a net security benefit for downstream consumers:
  they stop inheriting transitive dependencies (the Azure SDK, oras-go, casbin, go-redis and others)
  that they never use but must still track for CVEs.
* An unresolved follow-up: the API module will be tagged and released separately, so the release
  process must ensure it receives dependency bumps and security patches on the same cadence as the
  root module.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Import path changes break every downstream consumer. | Generated shims keep the old paths compiling for one release cycle. Migration is a mechanical find-and-replace, documented in the developer guide. |
| The credential methods cannot be preserved by the shims. | 22 in-repo call sites. Document prominently in release notes; confirm downstream impact before merging. |
| The enum type identity change cannot be preserved by the shims. | Affects only code that uses Argo CD's types *and* gitops-engine together. The compiler flags every site. |
| Consumers resolve incompatible Kubernetes versions. | Pin with `require`, never `replace`, since a dependency's `replace` is ignored. Verified by building a consumer with no replace directives. |
| The natively declared enums drift from gitops-engine's. | `test/apicompat` asserts the two constant sets agree in value, that the `OperationPhase` predicates behave identically, and that the two `ResourceKey` structs stay convertible. Drift fails CI rather than appearing at runtime. |
| Two modules drift in dependency versions. | Wired with a `replace` directive, exactly as `gitops-engine` is today, so local builds and CI always use the in-tree version. |
| Codegen breaks or silently half-runs. | `make codegen` was re-wired and verified idempotent. Note that a failed `gen_client` leaves `api/pkg/client` empty. |
| The split is partially done and leaves the graph no lighter. | Phases 1 and 2 are independently valuable and change no import paths; phase 3 only lands once the graph is already clean. |

### Upgrade / Downgrade Strategy

This change affects compile-time dependencies only. There is no runtime, cluster-state or CRD
impact, so no cluster-side upgrade or downgrade action is required.

For downstream consumers:

* **On upgrade, to keep previous behaviour:** nothing, for the release that carries the shims.
  Existing imports continue to compile. Two exceptions: a consumer calling
  `Repository.GetGitCreds`, `GetHelmCreds` or `GetOCICreds` must switch to the `util/repocreds`
  form, and a consumer that passes Argo CD's health or sync enum values directly into gitops-engine
  APIs must add an explicit conversion. Both are caught by the compiler.
* **On upgrade, to benefit from the enhancement:** change the import path from
  `github.com/argoproj/argo-cd/v3/pkg/apis/...` to `github.com/argoproj/argo-cd/api/v3/pkg/apis/...`,
  require the new module instead of the root module, and delete the copied `k8s.io/*` `replace`
  block from `go.mod`.
* **Downgrade:** revert the import path and restore the `replace` block. No state migration is
  involved.

Deprecation timeline: the shims ship in one minor release with a deprecation notice in the docs and
release notes, and are removed in the next major version.

## Drawbacks

* **It is a breaking change for every downstream consumer**, even with the shims, because the shims
  are temporary and both the credential methods and the enum type identity break immediately. The
  whole point of the proposal is to serve downstream consumers, so imposing a migration on them is
  a real cost that has to be weighed against the benefit.
* **The shims do not deliver the dependency benefit.** A consumer that keeps using the old paths
  still depends on the root module and still needs the `k8s.io/*` replace block. The benefit
  arrives only on the import-path change, which risks consumers believing they have upgraded when
  they have not.
* **Two modules are harder to maintain than one.** Dependency bumps, release tagging and CI all
  have to account for the extra module, and the API module's version pins are load-bearing in a way
  that is easy to undo with a careless `go mod tidy`. The repository already carries this cost for
  `gitops-engine`, so the marginal cost is lower than it would otherwise be, but it is not zero.
* **The change is large.** 538 files, of which 473 are import-path rewrites. It will conflict with
  essentially every in-flight pull request touching those files, so merge timing matters. The work
  should be split along the four phase boundaries rather than reviewed as one change.
* **`make codegen` became more complex**, since generation must target a different module than the
  one invoking it, and the proto files carry the module path in two places.
* The split is somewhat arbitrary at the margin. Deciding which utility helpers belong in the API
  module is a judgement call that will attract bikeshedding, and every helper pulled in risks
  re-growing the dependency graph.

## Alternatives

1. **Do nothing.** Downstream projects continue copying Argo CD's `replace` block. This is the
   status quo that #4055 and #14727 have been describing for years, and it effectively prevents
   consumers from choosing their own Kubernetes library versions.
2. **Go workspaces, as originally suggested in #14727.** A `go.work` file helps local multi-module
   development, but it is a development-time tool and changes nothing about what a published
   consumer resolves. It is complementary to this proposal rather than an alternative, and the
   repository currently manages `gitops-engine` with a `replace` directive and no `go.work`.
3. **Publish the API module from a separate repository**, as some projects do. This avoids the
   multi-module complexity in this repository, but it splits codegen across repositories and makes
   keeping the types in lockstep with the implementation significantly harder.
4. **Split light leaf packages out of gitops-engine** and have the API module type-alias to them,
   instead of declaring the enums natively. This preserves Go type identity exactly, so Argo CD
   needs no conversions and nothing breaks for projects using both. It was prototyped and works: a
   `kubetypes` leaf holding `ResourceKey` and the Kind constants, re-exported from
   `pkg/utils/kube` via aliases, removes `k8s.io/kubernetes` from `pkg/health` and
   `pkg/sync/common` (98 `k8s.io/kubernetes` packages to 0; 142 external dependency roots to 29).

   Rejected because it leaves the API module depending on gitops-engine. Downstream projects would
   still carry gitops-engine in their module graph for the sake of a handful of string constants,
   and the leaf split would have to land before the API module could. Declaring the enums natively
   costs conversions inside Argo CD but gives consumers a module that stands alone, which is the
   goal.
5. **Drop the `k8s.io/kubernetes` dependency from gitops-engine entirely.** This would let the
   root module delete its ~35 `k8s.io/*` replace directives, which is the bigger prize. It is not
   currently achievable: the dependency enters through `pkg/utils/kube/scheme`, which exists to
   provide `legacyscheme.Scheme`, and `pkg/diff` calls `Scheme.Default()` on it. Defaulting
   functions live only in `k8s.io/kubernetes/pkg/apis/*/v1/defaults.go` — `k8s.io/api` contains
   none, because the external type packages are pure type definitions. Substituting
   `k8s.io/client-go/kubernetes/scheme` would compile cleanly and silently turn `Default()` into a
   no-op, changing every diff that depends on server-side defaulting. Out of scope here, and worth
   its own issue.
6. **Keep the types where they are and ask consumers to use `unstructured` objects or their own
   copies of the types.** This loses type safety and the generated clientset, and copies drift.
   Several downstream projects already do this out of necessity, which is itself evidence of the
   problem.
