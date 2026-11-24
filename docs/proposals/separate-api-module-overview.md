---
title: Separating the Argo CD API — Overview
authors:
  - "@anandf"

creation-date: 2026-11-23
last-updated: 2026-11-23
---

# Separating the Argo CD API — Overview

This document explains what the change does, why it was needed, what made it difficult, and what
it costs. It is written for everyone who has to understand or approve the work, not only for Go
developers, so it explains the handful of concepts the argument depends on. The full engineering
design is in [separate-api-module.md](separate-api-module.md).

## Background: four terms

Only four concepts are needed to follow the rest.

**Module.** The unit Go code is published and versioned in. Argo CD ships two today: the main
module `github.com/argoproj/argo-cd/v3`, and `github.com/argoproj/argo-cd/gitops-engine/v3`, which
lives in the same repository under `./gitops-engine` and is tagged separately. The main module
depends on `gitops-engine`, so anything depending on Argo CD receives both. Neither is small, and
neither contains only the resource types.

**`go.mod`.** The file at the root of a module listing what it depends on and at which versions.

**Transitive dependency.** If project A depends on B, and B depends on C, then C is a transitive
dependency of A. A ends up carrying C whether or not it uses it directly.

**`replace` directive.** A line in `go.mod` that overrides where a dependency comes from or which
version is used. Crucially, Go only honours `replace` directives in the module you are actually
building — the *main* module. A `replace` inside a dependency is ignored. This detail turns out to
matter twice in this story.

## What the change does

It moves the Argo CD custom resource types (`Application`, `AppProject`, `ApplicationSet`) and the
generated Kubernetes client for them out of the main Argo CD module and into a new, separate
module:

```text
github.com/argoproj/argo-cd/api/v3
```

It lives in the same repository, under `./api`, alongside the `gitops-engine` module that is
already managed this way.

Projects that integrate with Argo CD can now depend on just that module. Three in our own ecosystem
need exactly this:

* **Red Hat GitOps Operator**
* **Argo CD Image Updater**
* **Argo CD Agent**

They create and read Argo CD resources. They do not run syncs, serve the API or talk to
repositories, so they have no use for most of what Argo CD contains.

## The problem

### 1. Integrators inherited the entire application controller

Argo CD's main module is sized for its binaries: the application controller, repo-server, API
server and CLI. It also depends on the `gitops-engine` module, so an integrator received that as
well. A project wanting only the resource types therefore pulled in **1,084 non-standard-library
packages** transitively, where the types themselves need about 343.

The surplus included an authorisation engine (casbin), an OCI registry client (oras-go), the Azure
SDK, a Redis client and a GitHub API client. None of it is reachable from the resource type
definitions in any meaningful sense; it arrived because of a handful of small references described
below.

### 2. Integrators were pinned to Argo CD's Kubernetes versions

Argo CD pins the Kubernetes client libraries it has tested against. Because an integrator had to
take the whole module, they inherited those pins — including for libraries they used directly for
their own unrelated purposes.

The practical consequence: an integrator could not move to a newer `client-go` or
`controller-runtime` until Argo CD did. Our release cadence became a constraint on theirs.

### 3. A fresh build did not work at all

This is the part most easily underestimated. The situation was not "builds, but heavy". For a new
integrator, `go mod tidy` **failed**:

```text
k8s.io/kubernetes/pkg/apis/core/validation imports
    k8s.io/kubelet/pkg/apis: reading k8s.io/kubelet/go.mod at revision v0.0.0:
    unknown revision v0.0.0
```

The cause is a well-known upstream issue. Argo CD's dependency graph reaches `k8s.io/kubernetes`,
the Kubernetes source tree itself. That module's `go.mod` refers to its own sub-repositories
(`k8s.io/kubelet`, `k8s.io/component-helpers` and around thirty others) at the placeholder version
`v0.0.0`, which was never published. Kubernetes resolves these internally with `replace`
directives.

Those `replace` directives are only honoured in the main module, so they do not reach anyone
depending on Argo CD. Every integrator therefore had to copy roughly **35** of them into their own
`go.mod` by hand, just to make the build resolve. That copying is also what caused problem 2.

Reported in [#4055](https://github.com/argoproj/argo-cd/issues/4055) in **2020**; the broader
request to separate the API in [#14727](https://github.com/argoproj/argo-cd/issues/14727) in
**2023**.

## What it delivers

Measured by building a real integrator project that imports `Application` and the typed clientset:

| | Before | After |
| --- | --- | --- |
| `replace` directives the integrator must copy and maintain | ~35 (mandatory) | **0** |
| Modules in the build list | 455 | 195 |
| Packages compiled | 1,084 | 343 |
| `k8s.io/kubernetes` packages reachable | 98 | **0** |
| `gitops-engine` packages reachable | 14 | **0** |
| Does a fresh `go mod tidy` succeed? | **No** | Yes |

Four consequences worth stating separately:

**Integrators are unblocked.** They choose their own Kubernetes library versions. We stop being an
involuntary gatekeeper on their release planning.

**Reduced vulnerability surface for them.** Every dependency a product ships must be monitored and
patched. Removing roughly two-thirds of them removes that ongoing obligation for dependencies they
never used.

**Faster builds.** Less to compile on every CI run, for every integrator.

**Lower support burden for us.** The `replace`-block workaround was undocumented knowledge passed
between teams. That class of question disappears.

> [!NOTE]
> Argo CD itself is unaffected. No behaviour, configuration, CRD schema or wire format changes, and
> nothing for Argo CD operators or users to do. The benefit is entirely for projects that build on
> top of Argo CD.

## Why it was difficult

### The types were coupled to the implementation

The resource type definitions are mostly plain data, but a small number of references reached into
Argo CD's machinery. Each was a few lines; each pulled a whole subsystem:

| Reference in the type definitions | What it pulled in |
| --- | --- |
| `GetGitCreds`, `GetHelmCreds`, `GetOCICreds` — convenience methods on `Repository` | go-git, Helm, oras-go, the Azure SDK and MSAL |
| Eight authorisation action names used in `AppProject` validation | casbin |
| Three annotation constants | go-redis, grpc, client-go |
| Two URL-handling helpers | the full Git and Helm clients |

The largest single contributor was different in kind. The health and sync status values
(`HealthStatusCode`, `SyncPhase`, `OperationPhase` and others) were not Argo CD's own — they were
defined by `gitops-engine`, a separate component. Those definitions sit in a package that
transitively registers the legacy Kubernetes scheme, and that is the **single** point where
`k8s.io/kubernetes` enters the graph.

Since those values appear as fields on `ApplicationStatus`, they are part of the public resource
API. We could not simply drop them; we had to declare our own equivalents. The string values are
identical, so nothing changes on the wire, but they are now distinct Go types, which means Argo CD
converts explicitly wherever it passes one to `gitops-engine`.

### Moving a package changes its import path everywhere

A nested module's import path must match its directory, so the path necessarily changed from
`github.com/argoproj/argo-cd/v3/pkg/apis/...` to `github.com/argoproj/argo-cd/api/v3/pkg/apis/...`.
Every reference across the repository had to be updated: **473 files**. Mechanical, but broad
enough to conflict with almost any other work in progress.

### Code generation had to be re-targeted

Much of this code is generated, not hand-written — protobuf definitions, deepcopy functions, the
OpenAPI schema, the CRD manifests and the typed client. The generators derive the import paths they
emit from whichever module they are run in, so running them unchanged produced wrong paths. Seven
separate places needed updating, including the `.proto` files, which embed the module path in two
different forms.

One subtlety only regeneration catches: the compiled protobuf file contains an embedded, compressed
schema descriptor. Editing the Go source leaves that descriptor stale — it still named
`gitops-engine` types in five places. Only running the generators removed them.

### Existing integrators could not simply be broken

Integrators have working code today. Changing an import path without a transition would break all
of them on upgrade.

### The trap that nearly shipped

This one is worth describing because it looked correct until it reached a consumer.

To keep Kubernetes versions consistent, the new module initially pinned them with `replace`
directives. Everything built correctly in our repository.

But as noted at the top: **`replace` directives in a dependency are ignored.** They applied to our
build and nobody else's. The first realistic integrator project resolved different versions of
`k8s.io/apimachinery` and `sigs.k8s.io/structured-merge-diff`, which are incompatible, and failed
to compile.

The fix was to express the constraints as `require` directives, which do travel. It was found only
because we built an integrator project from scratch and tried it, rather than treating our own
successful build as evidence the published module was usable.

## How it was done

Four phases. Each is independently reviewable and independently valuable, and the first two change
no import paths at all — so the work could be paused after either and still leave the project
better off.

The ordering is deliberate. Creating the new module first would have been the obvious opening move
and the wrong one: the types still dragged `gitops-engine`, casbin, oras-go and the Azure SDK
behind them, so the new module would have been as heavy as the old one. The types had to be made
light *before* they were worth moving.

### Phase 1 — Declare the status values natively

**Objective:** remove `k8s.io/kubernetes` from the graph.

Declare Argo CD's own `HealthStatusCode`, `SyncPhase`, `OperationPhase`, `ResultCode` and
`HookType` instead of borrowing `gitops-engine`'s, and convert explicitly at the points where Argo
CD hands these values to the sync engine. Those turned out to cluster at three places in the
controller rather than being spread throughout.

Because two definitions of the same values can drift apart, a test was added that compares them and
fails the build if they ever disagree. Without it, a mismatch would surface at runtime rather than
at build time.

**Result:** `k8s.io/kubernetes` went from 98 packages to zero.

### Phase 2 — Detach the remaining dependencies

**Objective:** remove the remaining heavyweight libraries.

Move the credential-construction methods off the `Repository` type into a package that owns those
dependencies, and split the small sets of constants and helpers out of the heavy packages they
happened to live in. In every case the original package re-exports what moved, so no existing
caller changed.

**Result:** casbin, oras-go, the Azure SDK, go-redis and the GitHub client all dropped out. The
types were now light, while still in the same module.

### Phase 3 — Create the module

**Objective:** give integrators something they can depend on alone.

Create `./api` with its own `go.mod`, move the types and generated client into it, update the 473
import references, pin the Kubernetes versions with `require`, and re-target code generation,
testing and linting for a two-module repository.

**Result:** an integrator project that depends only on the new module tidies, builds and runs with
zero `replace` directives.

### Phase 4 — Compatibility shims

**Objective:** give existing integrators a transition period instead of a hard break.

Generate re-export files at every import path that moved, so existing code continues to compile
unchanged. These are produced automatically from the real definitions, so they cannot drift. They
use Go *type aliases*, which means the old and new names refer to the same type and values can be
passed freely between code using either path.

**Result:** an integrator project written entirely against the old import paths compiles and runs
unchanged.

## The costs

**Integrators must eventually change an import path.** The shims are explicitly temporary: one
release cycle, removed in the next major version.

**The shims do not deliver the benefit.** This is the most important caveat to communicate. The
shims live in the main Argo CD module, so code using the old paths still depends on all of Argo CD
and still needs the `replace` block. The savings arrive only when an integrator actually changes
the import path. It would be easy to upgrade, see everything working, and wrongly conclude the
improvement had been obtained.

**Two changes the shims cannot cover.** The credential methods were removed from the `Repository`
type — Go does not allow re-attaching methods to a type defined elsewhere — and the status values
are now Argo CD's own types rather than `gitops-engine`'s. Both affect only integrators doing
deeper integration, and both produce clear compile errors pointing at the exact line rather than
silent misbehaviour.

**Two modules cost more to maintain than one.** Releases, version bumps and CI all have an
additional target, and the new module's version pins are load-bearing in a way that a careless
`go mod tidy` could undo. The repository already runs `gitops-engine` this way, so the pattern and
tooling exist.

**The change is large:** around 538 files, of which 473 are mechanical import updates. It should be
reviewed as the four phases above rather than as one change, and merged when it will collide least
with work in progress.

## Status and what is needed

The work is implemented and verified. Code generation runs cleanly and is idempotent, both modules
pass build, vet, lint and tests, and both scenarios were confirmed by building integrator projects
from scratch — one on the new import path, one on the old.

What remains is a set of decisions rather than engineering work:

1. Confirm the module path and that a nested module is preferred over the Go workspaces approach
   originally suggested in #14727.
2. Decide release timing: ship the shims in a v3 minor and remove the old paths in v4, or land the
   whole change in v4.
3. Decide whether the API module is tagged and released independently, as `gitops-engine` is.
4. Confirm with the three known integrators that neither of the two uncovered changes affects them.

These are listed in full at the top of [the technical proposal](separate-api-module.md).
