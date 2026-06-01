package graphcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNewTypeRelationshipCache(t *testing.T) {
	cache := NewTypeRelationshipCache()

	assert.NotNil(t, cache)
	assert.NotNil(t, cache.descendants)

	assert.True(t, len(cache.descendants) > 0, "Should have seeded relationships")

	deploymentGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	descendants := cache.GetDescendants(deploymentGVK)
	assert.Contains(t, descendants, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"})
}

func TestLearnRelationship(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Parent"}
	child := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Child"}

	cache.LearnRelationship(parent, child)

	descendants := cache.GetDescendants(parent)
	assert.Contains(t, descendants, child)
}

func TestLearnRelationship_MultipleChildren(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child1 := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child1"}
	child2 := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child2"}

	cache.LearnRelationship(parent, child1)
	cache.LearnRelationship(parent, child2)

	descendants := cache.GetDescendants(parent)
	assert.Len(t, descendants, 2)
	assert.Contains(t, descendants, child1)
	assert.Contains(t, descendants, child2)
}

func TestLearnRelationship_Duplicate(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}

	cache.LearnRelationship(parent, child)
	cache.LearnRelationship(parent, child)
	cache.LearnRelationship(parent, child)

	descendants := cache.GetDescendants(parent)
	assert.Len(t, descendants, 1)
	assert.Contains(t, descendants, child)
}

func TestGetDescendants_EmptyParent(t *testing.T) {
	cache := NewTypeRelationshipCache()

	unknownGVK := schema.GroupVersionKind{Group: "unknown.io", Version: "v1", Kind: "Unknown"}
	descendants := cache.GetDescendants(unknownGVK)

	assert.NotNil(t, descendants)
	assert.Len(t, descendants, 0)
}

func TestGetAllDescendantsRecursive(t *testing.T) {
	cache := NewTypeRelationshipCache()

	deployment := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	replicaSet := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}
	pod := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}

	cache.LearnRelationship(deployment, replicaSet)
	cache.LearnRelationship(replicaSet, pod)

	allDescendants := cache.GetAllDescendantsRecursive(deployment)

	assert.Contains(t, allDescendants, replicaSet)
	assert.Contains(t, allDescendants, pod)
	assert.Len(t, allDescendants, 2)
}

func TestGetAllDescendantsRecursive_AvoidsCycles(t *testing.T) {
	cache := NewTypeRelationshipCache()

	a := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "A"}
	b := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "B"}
	c := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "C"}

	cache.LearnRelationship(a, b)
	cache.LearnRelationship(b, c)
	cache.LearnRelationship(c, a)

	allDescendants := cache.GetAllDescendantsRecursive(a)

	assert.Contains(t, allDescendants, b)
	assert.Contains(t, allDescendants, c)
}

func TestGetAllLearnedRelationships(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child1 := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child1"}
	child2 := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child2"}

	cache.LearnRelationship(parent, child1)
	cache.LearnRelationship(parent, child2)

	allRels := cache.GetAllLearnedRelationships()

	found1 := false
	found2 := false
	for _, rel := range allRels {
		if rel.Parent == parent && rel.Child == child1 {
			found1 = true
		}
		if rel.Parent == parent && rel.Child == child2 {
			found2 = true
		}
	}
	assert.True(t, found1, "Should contain parent->child1 relationship")
	assert.True(t, found2, "Should contain parent->child2 relationship")
}

func TestHasDescendants(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}
	unknown := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Unknown"}

	cache.LearnRelationship(parent, child)

	assert.True(t, cache.HasDescendants(parent))
	assert.False(t, cache.HasDescendants(unknown))
}

func TestSeedWellKnownRelationships(t *testing.T) {
	cache := NewTypeRelationshipCache()

	testCases := []struct {
		name   string
		parent schema.GroupVersionKind
		child  schema.GroupVersionKind
	}{
		{
			name:   "Deployment -> ReplicaSet",
			parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			child:  schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
		},
		{
			name:   "ReplicaSet -> Pod",
			parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		},
		{
			name:   "StatefulSet -> Pod",
			parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"},
			child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		},
		{
			name:   "StatefulSet -> PVC",
			parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"},
			child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
		},
		{
			name:   "DaemonSet -> Pod",
			parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"},
			child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		},
		{
			name:   "Job -> Pod",
			parent: schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"},
			child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		},
		{
			name:   "CronJob -> Job",
			parent: schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"},
			child:  schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"},
		},
		{
			name:   "Service -> Endpoints",
			parent: schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"},
			child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Endpoints"},
		},
		{
			name:   "Service -> EndpointSlice",
			parent: schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"},
			child:  schema.GroupVersionKind{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSlice"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			descendants := cache.GetDescendants(tc.parent)
			assert.Contains(t, descendants, tc.child, "Expected %s to have child %s", tc.parent.String(), tc.child.String())
		})
	}
}

func TestConcurrentAccess(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}

	done := make(chan bool)

	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				cache.LearnRelationship(parent, child)
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				cache.GetDescendants(parent)
				cache.HasDescendants(parent)
			}
			done <- true
		}()
	}

	for i := 0; i < 20; i++ {
		<-done
	}

	descendants := cache.GetDescendants(parent)
	assert.Contains(t, descendants, child)
	assert.Len(t, descendants, 1)
}
