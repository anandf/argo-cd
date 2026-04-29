# Argo CD UI Performance Benchmark

Measures browser-side costs of processing the `/api/v1/applications` JSON response at scale: `JSON.parse()` time, heap allocation, client-side filtering, and sorting overhead.

## Prerequisites

- `kind` and `kubectl` installed
- `argocd` CLI installed
- Node.js (for Puppeteer-based automated runs)

## Setup

### 1. Create a Kind cluster

```bash
kind create cluster --name argocd-bench
```

### 2. Install Argo CD

```bash
kubectl create namespace argocd
kubectl apply -n argocd --server-side --force-conflicts \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
```

Wait for all pods to be ready:

```bash
kubectl wait --for=condition=available deployment -l app.kubernetes.io/part-of=argocd -n argocd --timeout=120s
```

### 3. Enable API key access and generate a token

```bash
# Allow the admin account to generate API keys
kubectl patch configmap/argocd-cm --type merge \
  -p '{"data":{"accounts.admin":"apiKey,login"}}' -n argocd

# Port-forward in the background
kubectl port-forward svc/argocd-server -n argocd 8443:443 &

# Get the admin password and login (non-interactive)
ARGOCD_PASSWORD=$(kubectl get secret argocd-initial-admin-secret -n argocd \
  -o jsonpath='{.data.password}' | base64 -d)
argocd login localhost:8443 --username admin --password "$ARGOCD_PASSWORD" --insecure

# Generate a bearer token for benchmarking
ARGOCD_TOKEN=$(argocd account generate-token)
echo "Token: $ARGOCD_TOKEN"
```

### 4. Create test applications

```bash
# Create 1000 applications via ApplicationSet
./hack/generate-scale-test-apps.sh 1000 argocd
```

### 5. Run the benchmark

**Automated (recommended):**

```bash
# Full workflow: create apps + benchmark
./hack/browser-perf-bench/run.sh \
  --count=1000 \
  --url=https://localhost:8443 \
  --token="$ARGOCD_TOKEN"

# If apps already exist, skip creation
./hack/browser-perf-bench/run.sh \
  --skip-create \
  --url=https://localhost:8443 \
  --token="$ARGOCD_TOKEN"
```

**Manual (browser):**

Launch Chrome in incognito mode with memory and GC instrumentation to ensure no cached JS, cookies, or service workers interfere:

```bash
# Close all Chrome windows first, then launch with a fresh profile.
# --disable-web-security only works with a separate --user-data-dir.
open -a "Google Chrome" --args \
  --enable-precise-memory-info \
  --js-flags="--expose-gc" \
  --disable-web-security \
  --user-data-dir=/tmp/bench-chrome \
  --ignore-certificate-errors
```

Then open `hack/browser-perf-bench/index.html`, enter your API URL and bearer token, and click **Run Benchmark**.

> **Note:** `--disable-web-security` is required because the page is loaded from `file://` (origin `null`), and the Argo CD API does not include `Access-Control-Allow-Origin` for that origin. This flag **only takes effect** when Chrome is launched with a dedicated `--user-data-dir` and no other Chrome windows are open. Always use `--incognito` to avoid cached JS/CSS from previous Argo CD versions.

## run.sh Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--count=N` | 1000 | Number of applications to create |
| `--namespace=NS` | argocd | Argo CD namespace |
| `--url=URL` | *(required)* | Argo CD API server URL |
| `--token=TOKEN` | *(required)* | Bearer token for authentication |
| `--skip-create` | false | Skip app creation (apps already exist) |
| `--cleanup` | false | Delete the ApplicationSet after the benchmark |
| `--iterations=N` | 3 | Iterations per measurement for averaging |
| `--no-headless` | false | Show the browser window |
| `--trace` | false | Capture a Chrome CPU profile |
| `--output=DIR` | `benchmark-results/<timestamp>` | Output directory |

## Output Files

Results are saved to `benchmark-results/<timestamp>/`:

| File | Description |
|------|-------------|
| `results.json` | Per-iteration data and averages |
| `results.csv` | Spreadsheet-ready format |
| `REPORT.md` | GitHub-ready markdown report |
| `api-response.json` | Raw API response used for measurement |
| `cpu-profile.cpuprofile` | Chrome DevTools CPU profile (with `--trace`) |

## Cleanup

```bash
# Delete the ApplicationSet (apps are removed automatically)
kubectl delete applicationset scale-test -n argocd

# Delete the destination namespaces (not auto-deleted)
kubectl get ns -o name | grep scale-ns- | xargs kubectl delete

# Stop the port-forward background process
kill %1

# Delete the Kind cluster entirely
kind delete cluster --name argocd-bench
```
