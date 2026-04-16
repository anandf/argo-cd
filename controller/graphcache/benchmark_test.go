package graphcache

import (
	"fmt"
	"sync"
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// BenchmarkResourceGraph_AddOrUpdate_SingleResource benchmarks adding a single resource
func BenchmarkResourceGraph_AddOrUpdate_SingleResource(b *testing.B) {
	graph := NewResourceGraph(32)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		node := &ResourceNode{
			Key: kube.ResourceKey{
				Group:     "apps",
				Kind:      "Deployment",
				Namespace: "default",
				Name:      fmt.Sprintf("deploy-%d", i),
			},
			ManagedBy: "app1",
			Info: &ResourceMetadata{
				Labels: map[string]string{
					"app.kubernetes.io/instance": "app1",
					"app":                        "myapp",
				},
			},
		}
		graph.AddOrUpdate(node)
	}
}

// BenchmarkResourceGraph_Get benchmarks looking up a single resource by key
func BenchmarkResourceGraph_Get(b *testing.B) {
	graph := NewResourceGraph(32)

	// Pre-populate with 10,000 resources
	for i := 0; i < 10000; i++ {
		graph.AddOrUpdate(&ResourceNode{
			Key: kube.ResourceKey{
				Group:     "apps",
				Kind:      "Deployment",
				Namespace: fmt.Sprintf("ns-%d", i%100),
				Name:      fmt.Sprintf("deploy-%d", i),
			},
			ManagedBy: fmt.Sprintf("app-%d", i%50),
			Info: &ResourceMetadata{
				Labels: map[string]string{
					"app.kubernetes.io/instance": fmt.Sprintf("app-%d", i%50),
				},
			},
		})
	}

	targetKey := kube.ResourceKey{
		Group:     "apps",
		Kind:      "Deployment",
		Namespace: "ns-50",
		Name:      "deploy-5050",
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		graph.Get(targetKey)
	}
}

// BenchmarkResourceGraph_GetByApplication benchmarks querying resources by application
func BenchmarkResourceGraph_GetByApplication(b *testing.B) {
	graph := NewResourceGraph(32)

	// Pre-populate: 50 apps, 200 resources each = 10,000 total
	for appIdx := 0; appIdx < 50; appIdx++ {
		appName := fmt.Sprintf("app-%d", appIdx)
		for resIdx := 0; resIdx < 200; resIdx++ {
			graph.AddOrUpdate(&ResourceNode{
				Key: kube.ResourceKey{
					Group:     "apps",
					Kind:      "Deployment",
					Namespace: "default",
					Name:      fmt.Sprintf("%s-deploy-%d", appName, resIdx),
				},
				ManagedBy: appName,
				Info: &ResourceMetadata{
					Labels: map[string]string{
						"app.kubernetes.io/instance": appName,
					},
				},
			})
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		graph.GetByApplication(fmt.Sprintf("app-%d", i%50))
	}
}

// BenchmarkResourceGraph_GetByType benchmarks querying resources by GroupKind
func BenchmarkResourceGraph_GetByType(b *testing.B) {
	graph := NewResourceGraph(32)

	types := []schema.GroupKind{
		{Group: "apps", Kind: "Deployment"},
		{Group: "apps", Kind: "ReplicaSet"},
		{Group: "", Kind: "Pod"},
		{Group: "", Kind: "Service"},
		{Group: "", Kind: "ConfigMap"},
	}

	// 2000 resources per type = 10,000 total
	for _, gk := range types {
		for i := 0; i < 2000; i++ {
			graph.AddOrUpdate(&ResourceNode{
				Key: kube.ResourceKey{
					Group:     gk.Group,
					Kind:      gk.Kind,
					Namespace: "default",
					Name:      fmt.Sprintf("%s-%d", gk.Kind, i),
				},
				ManagedBy: "app1",
			})
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		graph.GetByType(types[i%len(types)])
	}
}

// BenchmarkResourceGraph_GetByLabel benchmarks label-based lookups
func BenchmarkResourceGraph_GetByLabel(b *testing.B) {
	graph := NewResourceGraph(32)

	// 10,000 resources with varying label values
	for i := 0; i < 10000; i++ {
		graph.AddOrUpdate(&ResourceNode{
			Key: kube.ResourceKey{
				Group:     "apps",
				Kind:      "Deployment",
				Namespace: "default",
				Name:      fmt.Sprintf("deploy-%d", i),
			},
			ManagedBy: fmt.Sprintf("app-%d", i%50),
			Info: &ResourceMetadata{
				Labels: map[string]string{
					"app.kubernetes.io/instance": fmt.Sprintf("app-%d", i%50),
					"env":                        fmt.Sprintf("env-%d", i%5),
					"team":                       fmt.Sprintf("team-%d", i%10),
				},
			},
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		graph.GetByLabel("env", fmt.Sprintf("env-%d", i%5))
	}
}

// BenchmarkResourceGraph_Delete benchmarks deleting resources
func BenchmarkResourceGraph_Delete(b *testing.B) {
	graph := NewResourceGraph(32)

	// Pre-populate
	keys := make([]kube.ResourceKey, b.N)
	for i := 0; i < b.N; i++ {
		key := kube.ResourceKey{
			Group:     "apps",
			Kind:      "Deployment",
			Namespace: "default",
			Name:      fmt.Sprintf("deploy-%d", i),
		}
		keys[i] = key
		graph.AddOrUpdate(&ResourceNode{
			Key:       key,
			ManagedBy: "app1",
			Info: &ResourceMetadata{
				Labels: map[string]string{
					"app": "myapp",
				},
			},
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		graph.Delete(keys[i])
	}
}

// BenchmarkResourceGraph_HierarchyTraversal benchmarks traversing parent-child relationships
func BenchmarkResourceGraph_HierarchyTraversal(b *testing.B) {
	graph := NewResourceGraph(32)

	// Build a hierarchy: 100 Deployments -> 100 ReplicaSets -> 300 Pods
	for i := 0; i < 100; i++ {
		deployKey := kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: fmt.Sprintf("deploy-%d", i)}
		graph.AddOrUpdate(&ResourceNode{
			Key:       deployKey,
			UID:       fmt.Sprintf("deploy-uid-%d", i),
			ManagedBy: fmt.Sprintf("app-%d", i%10),
		})

		rsKey := kube.ResourceKey{Group: "apps", Kind: "ReplicaSet", Namespace: "default", Name: fmt.Sprintf("rs-%d", i)}
		graph.AddOrUpdate(&ResourceNode{
			Key: rsKey,
			UID: fmt.Sprintf("rs-uid-%d", i),
			Parents: []ParentRef{
				{ResourceKey: deployKey, UID: fmt.Sprintf("deploy-uid-%d", i)},
			},
		})

		for j := 0; j < 3; j++ {
			podKey := kube.ResourceKey{Kind: "Pod", Namespace: "default", Name: fmt.Sprintf("pod-%d-%d", i, j)}
			graph.AddOrUpdate(&ResourceNode{
				Key: podKey,
				UID: fmt.Sprintf("pod-uid-%d-%d", i, j),
				Parents: []ParentRef{
					{ResourceKey: rsKey, UID: fmt.Sprintf("rs-uid-%d", i)},
				},
			})
		}
	}

	rootKey := kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy-0"}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Traverse full hierarchy: Deployment -> ReplicaSet -> Pods
		children := graph.GetChildren(rootKey)
		for _, child := range children {
			graph.GetChildren(child.Key)
		}
	}
}

// BenchmarkResourceGraph_ConcurrentReadWrite benchmarks concurrent reads and writes
func BenchmarkResourceGraph_ConcurrentReadWrite(b *testing.B) {
	graph := NewResourceGraph(32)

	// Pre-populate with 5000 resources
	for i := 0; i < 5000; i++ {
		graph.AddOrUpdate(&ResourceNode{
			Key: kube.ResourceKey{
				Group:     "apps",
				Kind:      "Deployment",
				Namespace: fmt.Sprintf("ns-%d", i%50),
				Name:      fmt.Sprintf("deploy-%d", i),
			},
			ManagedBy: fmt.Sprintf("app-%d", i%25),
			Info: &ResourceMetadata{
				Labels: map[string]string{
					"app.kubernetes.io/instance": fmt.Sprintf("app-%d", i%25),
				},
			},
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			switch i % 4 {
			case 0: // Write
				graph.AddOrUpdate(&ResourceNode{
					Key: kube.ResourceKey{
						Group:     "apps",
						Kind:      "Deployment",
						Namespace: "default",
						Name:      fmt.Sprintf("concurrent-%d", i),
					},
					ManagedBy: "app-concurrent",
					Info: &ResourceMetadata{
						Labels: map[string]string{"app": "concurrent"},
					},
				})
			case 1: // Read by key
				graph.Get(kube.ResourceKey{
					Group:     "apps",
					Kind:      "Deployment",
					Namespace: fmt.Sprintf("ns-%d", i%50),
					Name:      fmt.Sprintf("deploy-%d", i%5000),
				})
			case 2: // Read by app
				graph.GetByApplication(fmt.Sprintf("app-%d", i%25))
			case 3: // Read by type
				graph.GetByType(schema.GroupKind{Group: "apps", Kind: "Deployment"})
			}
			i++
		}
	})
}

// BenchmarkResourceGraph_GetMetrics benchmarks metrics collection
func BenchmarkResourceGraph_GetMetrics(b *testing.B) {
	graph := NewResourceGraph(32)

	// Pre-populate with diverse resources
	types := []schema.GroupKind{
		{Group: "apps", Kind: "Deployment"},
		{Group: "apps", Kind: "ReplicaSet"},
		{Group: "", Kind: "Pod"},
		{Group: "", Kind: "Service"},
		{Group: "", Kind: "ConfigMap"},
		{Group: "", Kind: "Secret"},
	}

	for i := 0; i < 5000; i++ {
		gk := types[i%len(types)]
		graph.AddOrUpdate(&ResourceNode{
			Key: kube.ResourceKey{
				Group:     gk.Group,
				Kind:      gk.Kind,
				Namespace: fmt.Sprintf("ns-%d", i%20),
				Name:      fmt.Sprintf("res-%d", i),
			},
			ManagedBy: fmt.Sprintf("app-%d", i%30),
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		graph.GetMetrics()
	}
}

// BenchmarkResourceGraph_ShardComparison compares different shard counts
func BenchmarkResourceGraph_ShardComparison(b *testing.B) {
	shardCounts := []int{1, 8, 16, 32, 64, 128}

	for _, shards := range shardCounts {
		b.Run(fmt.Sprintf("Shards_%d", shards), func(b *testing.B) {
			graph := NewResourceGraph(shards)

			b.ResetTimer()
			b.ReportAllocs()

			var wg sync.WaitGroup
			workers := 8

			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()
					perWorker := b.N / workers
					for i := 0; i < perWorker; i++ {
						graph.AddOrUpdate(&ResourceNode{
							Key: kube.ResourceKey{
								Group:     "apps",
								Kind:      "Deployment",
								Namespace: fmt.Sprintf("ns-%d", (workerID*perWorker+i)%50),
								Name:      fmt.Sprintf("deploy-%d-%d", workerID, i),
							},
							ManagedBy: fmt.Sprintf("app-%d", i%25),
							Info: &ResourceMetadata{
								Labels: map[string]string{
									"app": fmt.Sprintf("app-%d", i%25),
								},
							},
						})
					}
				}(w)
			}
			wg.Wait()
		})
	}
}

// BenchmarkResourceGraph_ParentChildLookup benchmarks UID-based parent lookups
func BenchmarkResourceGraph_ParentChildLookup(b *testing.B) {
	graph := NewResourceGraph(32)

	// Create 1000 resources with UIDs
	for i := 0; i < 1000; i++ {
		graph.AddOrUpdate(&ResourceNode{
			Key: kube.ResourceKey{
				Group:     "apps",
				Kind:      "Deployment",
				Namespace: "default",
				Name:      fmt.Sprintf("deploy-%d", i),
			},
			UID:       fmt.Sprintf("uid-%d", i),
			ManagedBy: "app1",
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		graph.GetByUID(fmt.Sprintf("uid-%d", i%1000))
	}
}
