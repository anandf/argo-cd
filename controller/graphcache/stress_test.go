package graphcache

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestStress_LargeApplicationCount validates the graph with 1000+ applications
func TestStress_LargeApplicationCount(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	graph := NewResourceGraph(64)

	numApps := 1000
	resourcesPerApp := 9 // Deployment, Service, ConfigMap, Secret, ReplicaSet, 3 Pods, Endpoints

	start := time.Now()

	// Create resources for each application
	for appIdx := 0; appIdx < numApps; appIdx++ {
		appName := fmt.Sprintf("app-%d", appIdx)
		ns := fmt.Sprintf("ns-%d", appIdx%100)

		// Root resources: Deployment, Service, ConfigMap, Secret
		deployKey := kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: ns, Name: fmt.Sprintf("%s-deploy", appName)}
		graph.AddOrUpdate(&ResourceNode{
			Key:       deployKey,
			UID:       fmt.Sprintf("deploy-uid-%d", appIdx),
			ManagedBy: appName,
			Info: &ResourceMetadata{
				Labels: map[string]string{
					"app.kubernetes.io/instance": appName,
					"app":                        appName,
					"env":                        fmt.Sprintf("env-%d", appIdx%5),
				},
			},
		})

		svcKey := kube.ResourceKey{Kind: "Service", Namespace: ns, Name: fmt.Sprintf("%s-svc", appName)}
		graph.AddOrUpdate(&ResourceNode{
			Key:       svcKey,
			UID:       fmt.Sprintf("svc-uid-%d", appIdx),
			ManagedBy: appName,
			Info: &ResourceMetadata{
				Labels: map[string]string{
					"app.kubernetes.io/instance": appName,
				},
			},
		})

		graph.AddOrUpdate(&ResourceNode{
			Key:       kube.ResourceKey{Kind: "ConfigMap", Namespace: ns, Name: fmt.Sprintf("%s-config", appName)},
			UID:       fmt.Sprintf("cm-uid-%d", appIdx),
			ManagedBy: appName,
		})

		graph.AddOrUpdate(&ResourceNode{
			Key:       kube.ResourceKey{Kind: "Secret", Namespace: ns, Name: fmt.Sprintf("%s-secret", appName)},
			UID:       fmt.Sprintf("secret-uid-%d", appIdx),
			ManagedBy: appName,
		})

		// Child resources: ReplicaSet -> 3 Pods
		rsKey := kube.ResourceKey{Group: "apps", Kind: "ReplicaSet", Namespace: ns, Name: fmt.Sprintf("%s-rs", appName)}
		graph.AddOrUpdate(&ResourceNode{
			Key:       rsKey,
			UID:       fmt.Sprintf("rs-uid-%d", appIdx),
			ManagedBy: appName,
			Parents:   []ParentRef{{ResourceKey: deployKey, UID: fmt.Sprintf("deploy-uid-%d", appIdx)}},
		})

		for podIdx := 0; podIdx < 3; podIdx++ {
			graph.AddOrUpdate(&ResourceNode{
				Key:       kube.ResourceKey{Kind: "Pod", Namespace: ns, Name: fmt.Sprintf("%s-pod-%d", appName, podIdx)},
				UID:       fmt.Sprintf("pod-uid-%d-%d", appIdx, podIdx),
				ManagedBy: appName,
				Parents:   []ParentRef{{ResourceKey: rsKey, UID: fmt.Sprintf("rs-uid-%d", appIdx)}},
			})
		}

		// Endpoints (implicit child of Service)
		graph.AddOrUpdate(&ResourceNode{
			Key:       kube.ResourceKey{Kind: "Endpoints", Namespace: ns, Name: fmt.Sprintf("%s-svc", appName)},
			UID:       fmt.Sprintf("ep-uid-%d", appIdx),
			ManagedBy: appName,
			Parents:   []ParentRef{{ResourceKey: svcKey, UID: fmt.Sprintf("svc-uid-%d", appIdx)}},
		})
	}

	loadDuration := time.Since(start)

	// Validate
	totalExpected := numApps * resourcesPerApp
	assert.Equal(t, totalExpected, graph.Size(), "Expected %d resources, got %d", totalExpected, graph.Size())

	// Validate application index
	apps := graph.GetAllApplications()
	assert.Equal(t, numApps, len(apps), "Expected %d applications", numApps)

	// Validate per-app queries
	for i := 0; i < 10; i++ {
		appName := fmt.Sprintf("app-%d", i)
		resources := graph.GetByApplication(appName)
		assert.Equal(t, resourcesPerApp, len(resources), "App %s should have %d resources", appName, resourcesPerApp)
	}

	// Validate type index
	deployments := graph.GetByType(schema.GroupKind{Group: "apps", Kind: "Deployment"})
	assert.Equal(t, numApps, len(deployments))

	pods := graph.GetByType(schema.GroupKind{Kind: "Pod"})
	assert.Equal(t, numApps*3, len(pods))

	// Validate hierarchy
	deployKey := kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "ns-0", Name: "app-0-deploy"}
	children := graph.GetChildren(deployKey)
	assert.Equal(t, 1, len(children), "Deployment should have 1 ReplicaSet child")
	if len(children) > 0 {
		grandchildren := graph.GetChildren(children[0].Key)
		assert.Equal(t, 3, len(grandchildren), "ReplicaSet should have 3 Pod children")
	}

	// Performance assertions
	assert.Less(t, loadDuration.Seconds(), 5.0, "Loading %d resources should take < 5s, took %v", totalExpected, loadDuration)

	// Query performance
	queryStart := time.Now()
	for i := 0; i < 1000; i++ {
		graph.GetByApplication(fmt.Sprintf("app-%d", i))
	}
	queryDuration := time.Since(queryStart)
	assert.Less(t, queryDuration.Milliseconds(), int64(1000), "1000 app queries should take < 1s, took %v", queryDuration)

	t.Logf("Stress test results: %d resources loaded in %v, 1000 queries in %v", totalExpected, loadDuration, queryDuration)
}

// TestStress_HighConcurrency validates thread safety under extreme concurrency
func TestStress_HighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	graph := NewResourceGraph(32)

	numWorkers := runtime.NumCPU() * 4
	opsPerWorker := 5000
	var wg sync.WaitGroup
	var totalOps atomic.Int64

	start := time.Now()

	// Mix of operations: 40% write, 30% read by key, 20% read by app, 10% delete
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID)))

			for i := 0; i < opsPerWorker; i++ {
				op := rng.Intn(10)
				key := kube.ResourceKey{
					Group:     "apps",
					Kind:      "Deployment",
					Namespace: fmt.Sprintf("ns-%d", rng.Intn(50)),
					Name:      fmt.Sprintf("deploy-%d-%d", workerID, rng.Intn(100)),
				}

				switch {
				case op < 4: // 40% writes
					graph.AddOrUpdate(&ResourceNode{
						Key:       key,
						UID:       fmt.Sprintf("uid-%d-%d", workerID, i),
						ManagedBy: fmt.Sprintf("app-%d", rng.Intn(50)),
						Info: &ResourceMetadata{
							Labels: map[string]string{
								"app":     fmt.Sprintf("app-%d", rng.Intn(50)),
								"version": fmt.Sprintf("v%d", i),
							},
						},
					})
				case op < 7: // 30% read by key
					graph.Get(key)
				case op < 9: // 20% read by app
					graph.GetByApplication(fmt.Sprintf("app-%d", rng.Intn(50)))
				default: // 10% delete
					graph.Delete(key)
				}
				totalOps.Add(1)
			}
		}(w)
	}

	wg.Wait()
	duration := time.Since(start)

	ops := totalOps.Load()
	opsPerSec := float64(ops) / duration.Seconds()

	t.Logf("Concurrency stress: %d ops across %d workers in %v (%.0f ops/sec)",
		ops, numWorkers, duration, opsPerSec)

	// Throughput assertion: under normal conditions 50k+ ops/sec is expected.
	// Skip the assertion under -race since the race detector adds ~4x overhead.
	if !raceEnabled {
		assert.Greater(t, opsPerSec, 50000.0, "Should achieve > 50k ops/sec, got %.0f", opsPerSec)
	}
}

// TestStress_MemoryUsage validates memory stays reasonable at scale
func TestStress_MemoryUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	// Force GC before measurement
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	graph := NewResourceGraph(32)

	// Simulate 500 apps with 20 resources each = 10,000 resources
	numApps := 500
	resourcesPerApp := 20

	for appIdx := 0; appIdx < numApps; appIdx++ {
		appName := fmt.Sprintf("app-%d", appIdx)
		ns := fmt.Sprintf("ns-%d", appIdx%50)

		for resIdx := 0; resIdx < resourcesPerApp; resIdx++ {
			var gk schema.GroupKind
			switch resIdx % 5 {
			case 0:
				gk = schema.GroupKind{Group: "apps", Kind: "Deployment"}
			case 1:
				gk = schema.GroupKind{Group: "apps", Kind: "ReplicaSet"}
			case 2:
				gk = schema.GroupKind{Kind: "Pod"}
			case 3:
				gk = schema.GroupKind{Kind: "Service"}
			case 4:
				gk = schema.GroupKind{Kind: "ConfigMap"}
			}

			graph.AddOrUpdate(&ResourceNode{
				Key: kube.ResourceKey{
					Group:     gk.Group,
					Kind:      gk.Kind,
					Namespace: ns,
					Name:      fmt.Sprintf("%s-%s-%d", appName, gk.Kind, resIdx),
				},
				UID:       fmt.Sprintf("uid-%d-%d", appIdx, resIdx),
				ManagedBy: appName,
				Info: &ResourceMetadata{
					Labels: map[string]string{
						"app.kubernetes.io/instance":  appName,
						"app.kubernetes.io/component": gk.Kind,
						"env":                         fmt.Sprintf("env-%d", appIdx%3),
					},
					Annotations: map[string]string{
						"argocd.argoproj.io/tracking-id": fmt.Sprintf("%s:%s/%s:%s/%s-%s-%d",
							appName, gk.Group, gk.Kind, ns, appName, gk.Kind, resIdx),
					},
				},
			})
		}
	}

	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	totalResources := numApps * resourcesPerApp
	memUsedMB := float64(memAfter.Alloc-memBefore.Alloc) / 1024 / 1024
	bytesPerResource := float64(memAfter.Alloc-memBefore.Alloc) / float64(totalResources)

	assert.Equal(t, totalResources, graph.Size())

	t.Logf("Memory usage: %.2f MB for %d resources (%.0f bytes/resource)",
		memUsedMB, totalResources, bytesPerResource)

	// Memory should be reasonable — under 5KB per resource for metadata-only nodes
	// (includes labels, annotations, UIDs, graph indices, and Go map overhead)
	assert.Less(t, bytesPerResource, 5000.0,
		"Memory per resource should be < 5KB, got %.0f bytes", bytesPerResource)
}

// TestStress_RapidUpdates validates stability under rapid updates to the same resources
func TestStress_RapidUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	graph := NewResourceGraph(32)

	// Pre-create 100 resources
	numResources := 100
	keys := make([]kube.ResourceKey, numResources)
	for i := 0; i < numResources; i++ {
		key := kube.ResourceKey{
			Group:     "apps",
			Kind:      "Deployment",
			Namespace: "default",
			Name:      fmt.Sprintf("deploy-%d", i),
		}
		keys[i] = key
		graph.AddOrUpdate(&ResourceNode{
			Key:       key,
			UID:       fmt.Sprintf("uid-%d", i),
			ManagedBy: "app1",
			Info: &ResourceMetadata{
				Labels: map[string]string{"version": "v1"},
			},
		})
	}

	// Rapidly update all resources 1000 times
	numIterations := 1000
	var wg sync.WaitGroup

	start := time.Now()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for iter := 0; iter < numIterations; iter++ {
				for _, key := range keys {
					graph.AddOrUpdate(&ResourceNode{
						Key:       key,
						UID:       fmt.Sprintf("uid-%s", key.Name),
						ManagedBy: "app1",
						Info: &ResourceMetadata{
							Labels: map[string]string{
								"version":      fmt.Sprintf("v%d", iter),
								"last-updated": fmt.Sprintf("worker-%d-iter-%d", workerID, iter),
							},
						},
					})
				}
			}
		}(w)
	}
	wg.Wait()
	duration := time.Since(start)

	// Graph size should be stable (no duplicates)
	assert.Equal(t, numResources, graph.Size(), "Resource count should remain stable")

	// All resources should still be queryable
	for _, key := range keys {
		node, exists := graph.Get(key)
		require.True(t, exists, "Resource %v should still exist", key)
		assert.Equal(t, "app1", node.ManagedBy)
	}

	totalUpdates := 4 * numIterations * numResources
	t.Logf("Rapid update stress: %d total updates in %v (%.0f updates/sec)",
		totalUpdates, duration, float64(totalUpdates)/duration.Seconds())
}

// TestStress_LargeHierarchy validates deep and wide hierarchies
func TestStress_LargeHierarchy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	graph := NewResourceGraph(32)

	// Build a wide hierarchy: 100 Deployments, each with 3 ReplicaSets (rolling update),
	// each RS with 5 Pods = 100 + 300 + 1500 = 1900 resources
	numDeploys := 100
	rsPerDeploy := 3
	podsPerRS := 5

	for d := 0; d < numDeploys; d++ {
		deployKey := kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: fmt.Sprintf("deploy-%d", d)}
		graph.AddOrUpdate(&ResourceNode{
			Key:       deployKey,
			UID:       fmt.Sprintf("deploy-uid-%d", d),
			ManagedBy: fmt.Sprintf("app-%d", d%10),
		})

		for r := 0; r < rsPerDeploy; r++ {
			rsKey := kube.ResourceKey{Group: "apps", Kind: "ReplicaSet", Namespace: "default", Name: fmt.Sprintf("rs-%d-%d", d, r)}
			graph.AddOrUpdate(&ResourceNode{
				Key:     rsKey,
				UID:     fmt.Sprintf("rs-uid-%d-%d", d, r),
				Parents: []ParentRef{{ResourceKey: deployKey, UID: fmt.Sprintf("deploy-uid-%d", d)}},
			})

			for p := 0; p < podsPerRS; p++ {
				podKey := kube.ResourceKey{Kind: "Pod", Namespace: "default", Name: fmt.Sprintf("pod-%d-%d-%d", d, r, p)}
				graph.AddOrUpdate(&ResourceNode{
					Key:     podKey,
					UID:     fmt.Sprintf("pod-uid-%d-%d-%d", d, r, p),
					Parents: []ParentRef{{ResourceKey: rsKey, UID: fmt.Sprintf("rs-uid-%d-%d", d, r)}},
				})
			}
		}
	}

	totalExpected := numDeploys + numDeploys*rsPerDeploy + numDeploys*rsPerDeploy*podsPerRS
	assert.Equal(t, totalExpected, graph.Size())

	// Validate hierarchy traversal
	start := time.Now()
	for d := 0; d < numDeploys; d++ {
		deployKey := kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: fmt.Sprintf("deploy-%d", d)}
		children := graph.GetChildren(deployKey)
		assert.Equal(t, rsPerDeploy, len(children), "Deploy %d should have %d RS children", d, rsPerDeploy)

		totalPods := 0
		for _, rs := range children {
			pods := graph.GetChildren(rs.Key)
			totalPods += len(pods)
		}
		assert.Equal(t, rsPerDeploy*podsPerRS, totalPods, "Deploy %d should have %d total pods", d, rsPerDeploy*podsPerRS)
	}
	traversalDuration := time.Since(start)

	t.Logf("Large hierarchy: %d resources, full traversal of %d deployments in %v",
		totalExpected, numDeploys, traversalDuration)
	assert.Less(t, traversalDuration.Milliseconds(), int64(1000), "Full traversal should take < 1s")
}

// TestStress_DeleteAndRecreate validates stability when resources are rapidly deleted and recreated
func TestStress_DeleteAndRecreate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	graph := NewResourceGraph(32)
	numResources := 500
	numCycles := 50

	for cycle := 0; cycle < numCycles; cycle++ {
		// Create resources
		for i := 0; i < numResources; i++ {
			graph.AddOrUpdate(&ResourceNode{
				Key: kube.ResourceKey{
					Group:     "apps",
					Kind:      "Deployment",
					Namespace: "default",
					Name:      fmt.Sprintf("deploy-%d", i),
				},
				UID:       fmt.Sprintf("uid-cycle%d-%d", cycle, i),
				ManagedBy: fmt.Sprintf("app-%d", i%10),
				Info: &ResourceMetadata{
					Labels: map[string]string{
						"app":   fmt.Sprintf("app-%d", i%10),
						"cycle": fmt.Sprintf("%d", cycle),
					},
				},
			})
		}

		assert.Equal(t, numResources, graph.Size(), "Cycle %d: should have %d resources after creation", cycle, numResources)

		// Delete all resources
		for i := 0; i < numResources; i++ {
			graph.Delete(kube.ResourceKey{
				Group:     "apps",
				Kind:      "Deployment",
				Namespace: "default",
				Name:      fmt.Sprintf("deploy-%d", i),
			})
		}

		assert.Equal(t, 0, graph.Size(), "Cycle %d: should have 0 resources after deletion", cycle)
		assert.Empty(t, graph.GetAllApplications(), "Cycle %d: should have no applications after deletion", cycle)
	}
}
