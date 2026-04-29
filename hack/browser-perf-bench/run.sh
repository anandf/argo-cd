#!/bin/bash
# hack/browser-perf-bench/run.sh — Run the Argo CD UI performance benchmark
#
# Usage:
#   ./hack/browser-perf-bench/run.sh --count=1000 --url=https://localhost:8080 --token=<token>
#   ./hack/browser-perf-bench/run.sh --count=4000 --url=https://localhost:8080 --token=$(argocd account generate-token)
#   ./hack/browser-perf-bench/run.sh --count=1000 --no-headless --trace   # With visible browser + CPU profiles
#   ./hack/browser-perf-bench/run.sh --skip-create --url=https://localhost:8080 --token=<token>  # Apps already exist
#
# Prerequisites:
#   npm install puppeteer    (one-time)
#   A running Argo CD instance with kubectl access to the same cluster
#
# This script:
#   1. Creates real applications using hack/generate-scale-test-apps.sh (via ApplicationSet)
#   2. Runs the Puppeteer benchmark against the live Argo CD API
#   3. Optionally cleans up the applications after the benchmark
#
# Flags:
#   --count=N          Number of applications to create (default: 1000)
#   --namespace=NS     Argo CD namespace (default: argocd)
#   --url=URL          Argo CD API server URL (required)
#   --token=TOKEN      Argo CD bearer token (required)
#   --skip-create      Skip app creation (assume apps already exist)
#   --cleanup          Delete the ApplicationSet after the benchmark
#   --no-headless      Run with visible browser (useful for debugging)
#   --trace            Capture Chrome CPU profiles
#   --iterations=N     Iterations per measurement for averaging (default: 3)
#   --output=DIR       Output directory (default: benchmark-results/<timestamp>)
#
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
GENERATE_SCRIPT="$REPO_ROOT/hack/generate-scale-test-apps.sh"

# Defaults
COUNT=1000
NAMESPACE=argocd
URL=""
TOKEN=""
SKIP_CREATE=false
CLEANUP=false
EXTRA_ARGS=()

# Parse arguments
for arg in "$@"; do
  case "$arg" in
    --count=*)       COUNT="${arg#*=}" ;;
    --namespace=*)   NAMESPACE="${arg#*=}" ;;
    --url=*)         URL="${arg#*=}" ;;
    --token=*)       TOKEN="${arg#*=}" ;;
    --skip-create)   SKIP_CREATE=true ;;
    --cleanup)       CLEANUP=true ;;
    --help|-h)
      head -35 "$0" | tail -33
      exit 0
      ;;
    *)               EXTRA_ARGS+=("$arg") ;;
  esac
done

if [ -z "$URL" ]; then
  echo "Error: --url is required. Example: --url=https://localhost:8080"
  exit 1
fi
if [ -z "$TOKEN" ]; then
  echo "Error: --token is required. Example: --token=\$(argocd account generate-token)"
  exit 1
fi

# Check for puppeteer
if ! node -e "require('puppeteer')" 2>/dev/null; then
    echo "Puppeteer not found. Installing..."
    cd "$SCRIPT_DIR"
    npm install puppeteer
    echo ""
fi

echo "=== Argo CD UI Performance Benchmark ==="
echo ""
echo "  Apps:       $COUNT"
echo "  Namespace:  $NAMESPACE"
echo "  API URL:    $URL"
echo "  Skip create: $SKIP_CREATE"
echo "  Cleanup:    $CLEANUP"
echo ""

# Step 1: Create applications
if [ "$SKIP_CREATE" = false ]; then
  echo "--- Step 1: Creating $COUNT applications via ApplicationSet ---"
  echo ""
  bash "$GENERATE_SCRIPT" "$COUNT" "$NAMESPACE"
  echo ""

  echo "Waiting for applications to be ready..."
  TIMEOUT=120
  ELAPSED=0
  while true; do
    ACTUAL=$(kubectl get applications -n "$NAMESPACE" -l "app.kubernetes.io/managed-by=applicationset-controller" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "$ACTUAL" -ge "$COUNT" ]; then
      echo "All $COUNT applications created."
      break
    fi
    if [ "$ELAPSED" -ge "$TIMEOUT" ]; then
      echo "Warning: Only $ACTUAL/$COUNT applications created after ${TIMEOUT}s. Proceeding anyway."
      break
    fi
    sleep 5
    ELAPSED=$((ELAPSED + 5))
    echo "  $ACTUAL/$COUNT applications created (${ELAPSED}s elapsed)..."
  done
  echo ""
else
  echo "--- Step 1: Skipped (--skip-create) ---"
  ACTUAL=$(kubectl get applications -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  echo "Found $ACTUAL existing applications."
  echo ""
fi

# Step 2: Run the benchmark in live mode
echo "--- Step 2: Running benchmark against $URL ---"
echo ""

node "$SCRIPT_DIR/run-benchmark.mjs" \
  --live \
  --url="$URL" \
  --token="$TOKEN" \
  "${EXTRA_ARGS[@]}"

# Step 3: Cleanup (optional)
if [ "$CLEANUP" = true ]; then
  echo ""
  echo "--- Step 3: Cleaning up ---"
  kubectl delete applicationset scale-test -n "$NAMESPACE" --ignore-not-found
  echo "ApplicationSet deleted. Namespaces (scale-ns-*) are NOT auto-deleted."
  echo "To remove them: kubectl get ns -o name | grep scale-ns- | xargs kubectl delete"
fi
