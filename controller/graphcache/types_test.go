package graphcache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
)

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
