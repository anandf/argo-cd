package graphcache

import (
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
)

// BenchmarkResourceGraph_LabelUpdate_NoChanges benchmarks updating a resource with unchanged labels
// This is a common scenario where resources are frequently updated but labels rarely change
func BenchmarkResourceGraph_LabelUpdate_NoChanges(b *testing.B) {
	graph := NewResourceGraph(32)

	// Create a node with many labels
	node := &ResourceNode{
		Key: kube.ResourceKey{
			Group:     "apps",
			Kind:      "Deployment",
			Namespace: "default",
			Name:      "test-deploy",
		},
		ManagedBy: "app1",
		Info: &ResourceMetadata{
			Labels: map[string]string{
				"app":                          "myapp",
				"env":                          "production",
				"version":                      "v1.2.3",
				"team":                         "platform",
				"cost-center":                  "engineering",
				"app.kubernetes.io/name":       "myapp",
				"app.kubernetes.io/instance":   "myapp-prod",
				"app.kubernetes.io/version":    "1.2.3",
				"app.kubernetes.io/component":  "backend",
				"app.kubernetes.io/part-of":    "myapp-system",
				"app.kubernetes.io/managed-by": "argocd",
			},
		},
	}

	// Initial add
	graph.AddOrUpdate(node)

	b.ResetTimer()
	b.ReportAllocs()

	// Benchmark repeated updates with unchanged labels
	for i := 0; i < b.N; i++ {
		graph.AddOrUpdate(node)
	}
}

// BenchmarkResourceGraph_LabelUpdate_OneChanged benchmarks updating a resource with one label changed
func BenchmarkResourceGraph_LabelUpdate_OneChanged(b *testing.B) {
	graph := NewResourceGraph(32)

	node := &ResourceNode{
		Key: kube.ResourceKey{
			Group:     "apps",
			Kind:      "Deployment",
			Namespace: "default",
			Name:      "test-deploy",
		},
		ManagedBy: "app1",
		Info: &ResourceMetadata{
			Labels: map[string]string{
				"app":                          "myapp",
				"env":                          "production",
				"version":                      "v1.2.3",
				"team":                         "platform",
				"cost-center":                  "engineering",
				"app.kubernetes.io/name":       "myapp",
				"app.kubernetes.io/instance":   "myapp-prod",
				"app.kubernetes.io/version":    "1.2.3",
				"app.kubernetes.io/component":  "backend",
				"app.kubernetes.io/part-of":    "myapp-system",
				"app.kubernetes.io/managed-by": "argocd",
			},
		},
	}

	// Initial add
	graph.AddOrUpdate(node)

	b.ResetTimer()
	b.ReportAllocs()

	// Benchmark updates with one label changing
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			node.Info.Labels["version"] = "v1.2.4"
		} else {
			node.Info.Labels["version"] = "v1.2.3"
		}
		graph.AddOrUpdate(node)
	}
}

// BenchmarkResourceGraph_LabelUpdate_ManyLabels benchmarks updating with many label changes
func BenchmarkResourceGraph_LabelUpdate_ManyLabels(b *testing.B) {
	graph := NewResourceGraph(32)

	node := &ResourceNode{
		Key: kube.ResourceKey{
			Group:     "apps",
			Kind:      "Deployment",
			Namespace: "default",
			Name:      "test-deploy",
		},
		ManagedBy: "app1",
		Info: &ResourceMetadata{
			Labels: map[string]string{
				"app":                          "myapp",
				"env":                          "production",
				"version":                      "v1.2.3",
				"team":                         "platform",
				"cost-center":                  "engineering",
				"app.kubernetes.io/name":       "myapp",
				"app.kubernetes.io/instance":   "myapp-prod",
				"app.kubernetes.io/version":    "1.2.3",
				"app.kubernetes.io/component":  "backend",
				"app.kubernetes.io/part-of":    "myapp-system",
				"app.kubernetes.io/managed-by": "argocd",
			},
		},
	}

	// Initial add
	graph.AddOrUpdate(node)

	b.ResetTimer()
	b.ReportAllocs()

	// Benchmark updates with all labels changing
	for i := 0; i < b.N; i++ {
		for k := range node.Info.Labels {
			node.Info.Labels[k] = node.Info.Labels[k] + "-updated"
		}
		graph.AddOrUpdate(node)
	}
}

// BenchmarkResourceGraph_LabelUpdate_FewLabels benchmarks with resources having few labels (3)
func BenchmarkResourceGraph_LabelUpdate_FewLabels(b *testing.B) {
	graph := NewResourceGraph(32)

	node := &ResourceNode{
		Key: kube.ResourceKey{
			Group:     "apps",
			Kind:      "Deployment",
			Namespace: "default",
			Name:      "test-deploy",
		},
		ManagedBy: "app1",
		Info: &ResourceMetadata{
			Labels: map[string]string{
				"app": "myapp",
				"env": "production",
				"ver": "v1",
			},
		},
	}

	// Initial add
	graph.AddOrUpdate(node)

	b.ResetTimer()
	b.ReportAllocs()

	// Benchmark repeated updates with unchanged labels
	for i := 0; i < b.N; i++ {
		graph.AddOrUpdate(node)
	}
}

// BenchmarkResourceGraph_AddOrUpdate_Concurrent benchmarks concurrent updates
func BenchmarkResourceGraph_AddOrUpdate_Concurrent(b *testing.B) {
	graph := NewResourceGraph(32)

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			node := &ResourceNode{
				Key: kube.ResourceKey{
					Group:     "apps",
					Kind:      "Deployment",
					Namespace: "default",
					Name:      "test-deploy",
				},
				ManagedBy: "app1",
				Info: &ResourceMetadata{
					Labels: map[string]string{
						"app":     "myapp",
						"env":     "production",
						"version": "v1.2.3",
					},
				},
			}
			graph.AddOrUpdate(node)
			i++
		}
	})
}
