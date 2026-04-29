#!/bin/bash
# hack/bench-pagination.sh — Run pagination benchmarks and optionally compare with baseline
set -e

RESULTS_DIR="benchmark-results/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"

COUNT=${1:-3}
TIMEOUT=${2:-10m}

echo "=== Running pagination benchmarks (count=$COUNT, timeout=$TIMEOUT) ==="
echo ""

echo "--- Paginated vs Full List (10k apps) ---"
go test ./server/application/ \
  -bench='BenchmarkListPaginatedVsFull' \
  -benchmem -count="$COUNT" -timeout="$TIMEOUT" \
  2>&1 | tee "$RESULTS_DIR/paginated-vs-full.txt"

echo ""
echo "--- Cache Hit vs Miss (5k apps) ---"
go test ./server/application/ \
  -bench='BenchmarkListCacheHitVsMiss' \
  -benchmem -count="$COUNT" -timeout="$TIMEOUT" \
  2>&1 | tee "$RESULTS_DIR/cache-hit-vs-miss.txt"

echo ""
echo "--- Paginated First Page (10k apps, limit=50) ---"
go test ./server/application/ \
  -bench='BenchmarkListPaginated$' \
  -benchmem -count="$COUNT" -timeout="$TIMEOUT" \
  2>&1 | tee "$RESULTS_DIR/paginated-first-page.txt"

echo ""
echo "--- Paginated Page 10 (10k apps, offset=450, cache warm) ---"
go test ./server/application/ \
  -bench='BenchmarkListPaginatedPage10' \
  -benchmem -count="$COUNT" -timeout="$TIMEOUT" \
  2>&1 | tee "$RESULTS_DIR/paginated-page10.txt"

echo ""
echo "--- Walk All Pages via Continue Tokens (1k apps, limit=50) ---"
go test ./server/application/ \
  -bench='BenchmarkListPaginatedWalkAllPages' \
  -benchmem -count="$COUNT" -timeout="$TIMEOUT" \
  2>&1 | tee "$RESULTS_DIR/walk-all-pages.txt"

echo ""
echo "--- RBAC-Restricted User (5k apps, 100% visible) ---"
go test ./server/application/ \
  -bench='BenchmarkListWithRBACRoles' \
  -benchmem -count="$COUNT" -timeout="$TIMEOUT" \
  2>&1 | tee "$RESULTS_DIR/rbac-restricted.txt"

echo ""
echo "--- Existing Benchmarks (baseline comparison) ---"
go test ./server/application/ \
  -bench='BenchmarkList(Much|Some|Few)Apps$' \
  -benchmem -count="$COUNT" -timeout="$TIMEOUT" \
  2>&1 | tee "$RESULTS_DIR/existing-benchmarks.txt"

echo ""
echo "=== All results saved to $RESULTS_DIR/ ==="
echo ""
echo "To compare two runs:"
echo "  go install golang.org/x/perf/cmd/benchstat@latest"
echo "  benchstat <old-dir>/paginated-vs-full.txt $RESULTS_DIR/paginated-vs-full.txt"
