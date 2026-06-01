package graphcache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
)

func TestDefaultGraphConfig(t *testing.T) {
	cfg := DefaultGraphConfig()

	assert.Equal(t, 5*time.Minute, cfg.DiscoveryInterval, "Default discovery interval should be 5m")
	assert.Equal(t, 10*time.Second, cfg.InitialDiscoveryDelay, "Default initial discovery delay should be 10s")
	assert.Equal(t, 30*time.Second, cfg.MetricsExportInterval, "Default metrics export interval should be 30s")
	assert.Equal(t, 1*time.Minute, cfg.PersistenceInterval, "Default persistence interval should be 1m")
	assert.Equal(t, 100, cfg.MaxConsecutiveFailures, "Default max consecutive failures should be 100")
	assert.Equal(t, 1*time.Second, cfg.MinRetryInterval, "Default min retry interval should be 1s")
	assert.Equal(t, 30*time.Second, cfg.MaxRetryInterval, "Default max retry interval should be 30s")
}

func TestNewResourceGraph(t *testing.T) {
	graph := NewResourceGraph()
	assert.NotNil(t, graph)
	assert.Equal(t, 0, graph.Size())
}

func TestResourceGraph_AddOrUpdate(t *testing.T) {
	graph := NewResourceGraph()

	node := &ResourceNode{
		Key: kube.ResourceKey{
			Group:     "apps",
			Kind:      "Deployment",
			Namespace: "default",
			Name:      "test-deployment",
		},
		ManagedBy:  "guestbook",
		TrackingID: "guestbook:apps/Deployment:default/test-deployment",
		Info:       &ResourceMetadata{},
	}

	// Add new node
	graph.AddOrUpdate(node)

	assert.Equal(t, 1, graph.Size())
	retrieved, exists := graph.Get(node.Key)
	assert.True(t, exists)
	assert.Equal(t, node.ManagedBy, retrieved.ManagedBy)
	assert.Equal(t, node.TrackingID, retrieved.TrackingID)
	assert.False(t, retrieved.CreatedAt.IsZero())
	assert.False(t, retrieved.UpdatedAt.IsZero())

	// Update existing node
	originalCreatedAt := retrieved.CreatedAt
	time.Sleep(10 * time.Millisecond)

	node.ManagedBy = "guestbook-v2"
	graph.AddOrUpdate(node)

	assert.Equal(t, 1, graph.Size()) // Still only 1 node
	retrieved, exists = graph.Get(node.Key)
	assert.True(t, exists)
	assert.Equal(t, "guestbook-v2", retrieved.ManagedBy)
	assert.Equal(t, originalCreatedAt, retrieved.CreatedAt)      // CreatedAt unchanged
	assert.True(t, retrieved.UpdatedAt.After(originalCreatedAt)) // UpdatedAt changed
}

func TestResourceGraph_Delete(t *testing.T) {
	graph := NewResourceGraph()

	node := &ResourceNode{
		Key: kube.ResourceKey{
			Group:     "apps",
			Kind:      "Deployment",
			Namespace: "default",
			Name:      "test-deployment",
		},
		ManagedBy: "guestbook",
	}

	graph.AddOrUpdate(node)
	assert.Equal(t, 1, graph.Size())

	graph.Delete(node.Key)
	assert.Equal(t, 0, graph.Size())

	_, exists := graph.Get(node.Key)
	assert.False(t, exists)
}

func TestResourceGraph_GetByApplication(t *testing.T) {
	graph := NewResourceGraph()

	// Add multiple resources for different applications
	nodes := []*ResourceNode{
		{
			Key:       kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy1"},
			ManagedBy: "app1",
		},
		{
			Key:       kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy2"},
			ManagedBy: "app1",
		},
		{
			Key:       kube.ResourceKey{Group: "", Kind: "Service", Namespace: "default", Name: "svc1"},
			ManagedBy: "app2",
		},
	}

	for _, node := range nodes {
		graph.AddOrUpdate(node)
	}

	// Get resources for app1
	app1Resources := graph.GetByApplication("app1")
	assert.Equal(t, 2, len(app1Resources))

	// Get resources for app2
	app2Resources := graph.GetByApplication("app2")
	assert.Equal(t, 1, len(app2Resources))

	// Get resources for non-existent app
	app3Resources := graph.GetByApplication("app3")
	assert.Equal(t, 0, len(app3Resources))
}

func TestResourceGraph_GetByType(t *testing.T) {
	graph := NewResourceGraph()

	deploymentGK := schema.GroupKind{Group: "apps", Kind: "Deployment"}
	serviceGK := schema.GroupKind{Group: "", Kind: "Service"}

	nodes := []*ResourceNode{
		{
			Key:       kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy1"},
			ManagedBy: "app1",
		},
		{
			Key:       kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy2"},
			ManagedBy: "app1",
		},
		{
			Key:       kube.ResourceKey{Group: "", Kind: "Service", Namespace: "default", Name: "svc1"},
			ManagedBy: "app2",
		},
	}

	for _, node := range nodes {
		graph.AddOrUpdate(node)
	}

	deployments := graph.GetByType(deploymentGK)
	assert.Equal(t, 2, len(deployments))

	services := graph.GetByType(serviceGK)
	assert.Equal(t, 1, len(services))
}

func TestResourceGraph_ParentChildRelationships(t *testing.T) {
	graph := NewResourceGraph()

	deploymentKey := kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy1"}
	replicaSetKey := kube.ResourceKey{Group: "apps", Kind: "ReplicaSet", Namespace: "default", Name: "rs1"}
	podKey := kube.ResourceKey{Group: "", Kind: "Pod", Namespace: "default", Name: "pod1"}

	// Add deployment
	deployment := &ResourceNode{
		Key:       deploymentKey,
		ManagedBy: "app1",
		Children:  []kube.ResourceKey{},
	}
	graph.AddOrUpdate(deployment)

	// Add replicaset with deployment as parent
	replicaSet := &ResourceNode{
		Key:       replicaSetKey,
		ManagedBy: "app1",
		Parents:   []ParentRef{{ResourceKey: deploymentKey}},
		Children:  []kube.ResourceKey{},
	}
	graph.AddOrUpdate(replicaSet)

	// Add pod with replicaset as parent
	pod := &ResourceNode{
		Key:       podKey,
		ManagedBy: "app1",
		Parents:   []ParentRef{{ResourceKey: replicaSetKey}},
	}
	graph.AddOrUpdate(pod)

	// Verify parent-child relationships
	children := graph.GetChildren(deploymentKey)
	assert.Equal(t, 1, len(children))
	assert.Equal(t, replicaSetKey, children[0].Key)

	parents := graph.GetParents(replicaSetKey)
	assert.Equal(t, 1, len(parents))
	assert.Equal(t, deploymentKey, parents[0].Key)

	rsChildren := graph.GetChildren(replicaSetKey)
	assert.Equal(t, 1, len(rsChildren))
	assert.Equal(t, podKey, rsChildren[0].Key)

	podParents := graph.GetParents(podKey)
	assert.Equal(t, 1, len(podParents))
	assert.Equal(t, replicaSetKey, podParents[0].Key)
}

func TestResourceGraph_GetAllTypes(t *testing.T) {
	graph := NewResourceGraph()

	nodes := []*ResourceNode{
		{Key: kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy1"}},
		{Key: kube.ResourceKey{Group: "", Kind: "Service", Namespace: "default", Name: "svc1"}},
		{Key: kube.ResourceKey{Group: "apps", Kind: "StatefulSet", Namespace: "default", Name: "sts1"}},
	}

	for _, node := range nodes {
		graph.AddOrUpdate(node)
	}

	types := graph.GetAllTypes()
	assert.Equal(t, 3, len(types))

	// Verify expected types are present
	typeMap := make(map[schema.GroupKind]bool)
	for _, gk := range types {
		typeMap[gk] = true
	}

	assert.True(t, typeMap[schema.GroupKind{Group: "apps", Kind: "Deployment"}])
	assert.True(t, typeMap[schema.GroupKind{Group: "", Kind: "Service"}])
	assert.True(t, typeMap[schema.GroupKind{Group: "apps", Kind: "StatefulSet"}])
}

func TestResourceGraph_GetAllApplications(t *testing.T) {
	graph := NewResourceGraph()

	nodes := []*ResourceNode{
		{Key: kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy1"}, ManagedBy: "app1"},
		{Key: kube.ResourceKey{Group: "", Kind: "Service", Namespace: "default", Name: "svc1"}, ManagedBy: "app2"},
		{Key: kube.ResourceKey{Group: "apps", Kind: "StatefulSet", Namespace: "default", Name: "sts1"}, ManagedBy: "app3"},
	}

	for _, node := range nodes {
		graph.AddOrUpdate(node)
	}

	apps := graph.GetAllApplications()
	assert.Equal(t, 3, len(apps))

	appMap := make(map[string]bool)
	for _, app := range apps {
		appMap[app] = true
	}

	assert.True(t, appMap["app1"])
	assert.True(t, appMap["app2"])
	assert.True(t, appMap["app3"])
}

func TestResourceGraph_GetMetrics(t *testing.T) {
	graph := NewResourceGraph()

	nodes := []*ResourceNode{
		{Key: kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy1"}, ManagedBy: "app1"},
		{Key: kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy2"}, ManagedBy: "app1"},
		{Key: kube.ResourceKey{Group: "", Kind: "Service", Namespace: "default", Name: "svc1"}, ManagedBy: "app2"},
	}

	for _, node := range nodes {
		graph.AddOrUpdate(node)
	}

	metrics := graph.GetMetrics()

	assert.Equal(t, 3, metrics.TotalResources)
	assert.Equal(t, 2, metrics.UniqueApplications)
	assert.Equal(t, 2, metrics.UniqueResourceTypes)
	assert.Equal(t, 2, metrics.ResourcesByType[schema.GroupKind{Group: "apps", Kind: "Deployment"}])
	assert.Equal(t, 1, metrics.ResourcesByType[schema.GroupKind{Group: "", Kind: "Service"}])
	assert.Equal(t, 2, metrics.ResourcesByApp["app1"])
	assert.Equal(t, 1, metrics.ResourcesByApp["app2"])
}

func TestResourceGraph_DeleteWithRelationships(t *testing.T) {
	graph := NewResourceGraph()

	parentKey := kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "deploy1"}
	childKey := kube.ResourceKey{Group: "apps", Kind: "ReplicaSet", Namespace: "default", Name: "rs1"}

	parent := &ResourceNode{
		Key:       parentKey,
		ManagedBy: "app1",
		Children:  []kube.ResourceKey{},
	}
	graph.AddOrUpdate(parent)

	child := &ResourceNode{
		Key:       childKey,
		ManagedBy: "app1",
		Parents:   []ParentRef{{ResourceKey: parentKey}},
	}
	graph.AddOrUpdate(child)

	// Verify relationship exists
	children := graph.GetChildren(parentKey)
	require.Equal(t, 1, len(children))

	// Delete child
	graph.Delete(childKey)

	// Verify child is gone
	_, exists := graph.Get(childKey)
	assert.False(t, exists)

	// Verify parent's children list is updated
	children = graph.GetChildren(parentKey)
	assert.Equal(t, 0, len(children))
}

func TestResourceGraph_ConcurrentAccess(t *testing.T) {
	graph := NewResourceGraph()

	// Test concurrent adds
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(index int) {
			node := &ResourceNode{
				Key: kube.ResourceKey{
					Group:     "apps",
					Kind:      "Deployment",
					Namespace: "default",
					Name:      "deploy" + string(rune(index)),
				},
				ManagedBy: "app1",
			}
			graph.AddOrUpdate(node)
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	assert.Equal(t, 10, graph.Size())

	// Test concurrent reads
	for i := 0; i < 10; i++ {
		go func() {
			_ = graph.GetAllApplications()
			_ = graph.GetAllTypes()
			_ = graph.GetMetrics()
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestResourceGraph_LabelIndexOptimization tests the optimized label index update logic
func TestResourceGraph_LabelIndexOptimization(t *testing.T) {
	graph := NewResourceGraph()

	t.Run("add node with labels", func(t *testing.T) {
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
					"env":     "prod",
					"version": "v1",
					"team":    "platform",
				},
			},
		}

		graph.AddOrUpdate(node)

		// Verify all labels are indexed
		envNodes := graph.GetByLabel("env", "prod")
		assert.Len(t, envNodes, 1)
		assert.Equal(t, "test-deploy", envNodes[0].Key.Name)

		versionNodes := graph.GetByLabel("version", "v1")
		assert.Len(t, versionNodes, 1)

		teamNodes := graph.GetByLabel("team", "platform")
		assert.Len(t, teamNodes, 1)
	})

	t.Run("update with unchanged labels", func(t *testing.T) {
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
					"env":     "prod",
					"version": "v1",
					"team":    "platform",
				},
			},
		}

		// Update with same labels
		graph.AddOrUpdate(node)

		// Verify labels still indexed correctly
		envNodes := graph.GetByLabel("env", "prod")
		assert.Len(t, envNodes, 1)
		versionNodes := graph.GetByLabel("version", "v1")
		assert.Len(t, versionNodes, 1)
		teamNodes := graph.GetByLabel("team", "platform")
		assert.Len(t, teamNodes, 1)
	})

	t.Run("update with changed label value", func(t *testing.T) {
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
					"env":     "staging", // Changed from prod to staging
					"version": "v1",
					"team":    "platform",
				},
			},
		}

		graph.AddOrUpdate(node)

		// Old value should not be indexed
		prodNodes := graph.GetByLabel("env", "prod")
		assert.Len(t, prodNodes, 0)

		// New value should be indexed
		stagingNodes := graph.GetByLabel("env", "staging")
		assert.Len(t, stagingNodes, 1)
		assert.Equal(t, "test-deploy", stagingNodes[0].Key.Name)

		// Unchanged labels still indexed
		versionNodes := graph.GetByLabel("version", "v1")
		assert.Len(t, versionNodes, 1)
	})

	t.Run("update with added labels", func(t *testing.T) {
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
					"env":      "staging",
					"version":  "v1",
					"team":     "platform",
					"replicas": "3", // New label
				},
			},
		}

		graph.AddOrUpdate(node)

		// New label should be indexed
		replicaNodes := graph.GetByLabel("replicas", "3")
		assert.Len(t, replicaNodes, 1)
		assert.Equal(t, "test-deploy", replicaNodes[0].Key.Name)

		// Existing labels still indexed
		stagingNodes := graph.GetByLabel("env", "staging")
		assert.Len(t, stagingNodes, 1)
	})

	t.Run("update with removed labels", func(t *testing.T) {
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
					"env":     "staging",
					"version": "v1",
					// "team" removed
					// "replicas" removed
				},
			},
		}

		graph.AddOrUpdate(node)

		// Removed labels should not be indexed
		teamNodes := graph.GetByLabel("team", "platform")
		assert.Len(t, teamNodes, 0)
		replicaNodes := graph.GetByLabel("replicas", "3")
		assert.Len(t, replicaNodes, 0)

		// Remaining labels still indexed
		stagingNodes := graph.GetByLabel("env", "staging")
		assert.Len(t, stagingNodes, 1)
		versionNodes := graph.GetByLabel("version", "v1")
		assert.Len(t, versionNodes, 1)
	})

	t.Run("update with mixed changes", func(t *testing.T) {
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
					"env":      "prod",      // Changed back from staging to prod
					"version":  "v2",        // Changed from v1 to v2
					"region":   "us-west-2", // New label
					// "version" kept but value changed
				},
			},
		}

		graph.AddOrUpdate(node)

		// Changed labels should reflect new values
		prodNodes := graph.GetByLabel("env", "prod")
		assert.Len(t, prodNodes, 1)
		stagingNodes := graph.GetByLabel("env", "staging")
		assert.Len(t, stagingNodes, 0)

		v2Nodes := graph.GetByLabel("version", "v2")
		assert.Len(t, v2Nodes, 1)
		v1Nodes := graph.GetByLabel("version", "v1")
		assert.Len(t, v1Nodes, 0)

		// New label should be indexed
		regionNodes := graph.GetByLabel("region", "us-west-2")
		assert.Len(t, regionNodes, 1)
	})

	t.Run("update removes all labels", func(t *testing.T) {
		node := &ResourceNode{
			Key: kube.ResourceKey{
				Group:     "apps",
				Kind:      "Deployment",
				Namespace: "default",
				Name:      "test-deploy",
			},
			ManagedBy: "app1",
			Info: &ResourceMetadata{
				Labels: map[string]string{}, // All labels removed
			},
		}

		graph.AddOrUpdate(node)

		// All labels should be removed from index
		prodNodes := graph.GetByLabel("env", "prod")
		assert.Len(t, prodNodes, 0)
		v2Nodes := graph.GetByLabel("version", "v2")
		assert.Len(t, v2Nodes, 0)
		regionNodes := graph.GetByLabel("region", "us-west-2")
		assert.Len(t, regionNodes, 0)

		// Node should still exist in graph
		retrieved, exists := graph.Get(node.Key)
		assert.True(t, exists)
		assert.Equal(t, "test-deploy", retrieved.Key.Name)
	})
}
