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
	assert.NotNil(t, cache.confidence)

	// Should have well-known relationships seeded
	assert.True(t, len(cache.descendants) > 0, "Should have seeded relationships")

	// Check specific well-known relationship: Deployment -> ReplicaSet
	deploymentGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	descendants := cache.GetDescendants(deploymentGVK)
	assert.Contains(t, descendants, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"})
}

func TestLearnRelationship(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Parent"}
	child := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Child"}

	// Learn a new relationship
	cache.LearnRelationship(parent, child, 1)

	// Verify it was learned
	descendants := cache.GetDescendants(parent)
	assert.Contains(t, descendants, child)

	// Verify confidence
	confidence := cache.GetConfidence(parent, child)
	assert.Equal(t, 1, confidence)

	// Learn it again to increase confidence
	cache.LearnRelationship(parent, child, 1)
	confidence = cache.GetConfidence(parent, child)
	assert.Equal(t, 2, confidence)
}

func TestLearnRelationship_MultipleChildren(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child1 := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child1"}
	child2 := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child2"}

	cache.LearnRelationship(parent, child1, 1)
	cache.LearnRelationship(parent, child2, 1)

	descendants := cache.GetDescendants(parent)
	assert.Len(t, descendants, 2)
	assert.Contains(t, descendants, child1)
	assert.Contains(t, descendants, child2)
}

func TestLearnRelationship_Duplicate(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}

	// Learn the same relationship multiple times
	cache.LearnRelationship(parent, child, 1)
	cache.LearnRelationship(parent, child, 1)
	cache.LearnRelationship(parent, child, 1)

	// Should only appear once in descendants list
	descendants := cache.GetDescendants(parent)
	assert.Len(t, descendants, 1)
	assert.Contains(t, descendants, child)

	// But confidence should accumulate
	confidence := cache.GetConfidence(parent, child)
	assert.Equal(t, 3, confidence)
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

	// Create a chain: Deployment -> ReplicaSet -> Pod
	deployment := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	replicaSet := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}
	pod := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}

	cache.LearnRelationship(deployment, replicaSet, 1)
	cache.LearnRelationship(replicaSet, pod, 1)

	// Get all descendants recursively
	allDescendants := cache.GetAllDescendantsRecursive(deployment)

	assert.Contains(t, allDescendants, replicaSet)
	assert.Contains(t, allDescendants, pod)
	assert.Len(t, allDescendants, 2)
}

func TestGetAllDescendantsRecursive_AvoidsCycles(t *testing.T) {
	cache := NewTypeRelationshipCache()

	// Create a cycle: A -> B -> C -> A
	a := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "A"}
	b := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "B"}
	c := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "C"}

	cache.LearnRelationship(a, b, 1)
	cache.LearnRelationship(b, c, 1)
	cache.LearnRelationship(c, a, 1) // Creates cycle

	// Should not infinite loop
	allDescendants := cache.GetAllDescendantsRecursive(a)

	// Should contain B and C but not enter infinite recursion
	assert.Contains(t, allDescendants, b)
	assert.Contains(t, allDescendants, c)
}

func TestGetAllRelationships(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child1 := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child1"}
	child2 := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child2"}

	cache.LearnRelationship(parent, child1, 5)
	cache.LearnRelationship(parent, child2, 3)

	allRels := cache.GetAllRelationships()

	// Check that both relationships are present
	rel1 := TypeRelationship{Parent: parent, Child: child1}
	rel2 := TypeRelationship{Parent: parent, Child: child2}

	confidence1, exists1 := allRels[rel1]
	confidence2, exists2 := allRels[rel2]

	assert.True(t, exists1)
	assert.True(t, exists2)
	assert.Equal(t, 5, confidence1)
	assert.Equal(t, 3, confidence2)
}

func TestHasDescendants(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}
	unknown := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Unknown"}

	cache.LearnRelationship(parent, child, 1)

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

			// Well-known relationships should have high confidence
			confidence := cache.GetConfidence(tc.parent, tc.child)
			assert.Greater(t, confidence, 50, "Well-known relationships should have high confidence")
		})
	}
}

func TestConcurrentAccess(t *testing.T) {
	cache := NewTypeRelationshipCache()

	parent := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	child := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}

	// Simulate concurrent learning and reading
	done := make(chan bool)

	// Writer goroutines
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				cache.LearnRelationship(parent, child, 1)
			}
			done <- true
		}()
	}

	// Reader goroutines
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				cache.GetDescendants(parent)
				cache.GetConfidence(parent, child)
				cache.HasDescendants(parent)
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 20; i++ {
		<-done
	}

	// Verify the relationship was learned
	descendants := cache.GetDescendants(parent)
	assert.Contains(t, descendants, child)
	assert.Equal(t, 1000, cache.GetConfidence(parent, child))
}

func TestRelationshipKey(t *testing.T) {
	parent := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	child := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}

	key := relationshipKey(parent, child)

	assert.NotEmpty(t, key)
	assert.Contains(t, key, "Deployment")
	assert.Contains(t, key, "ReplicaSet")

	// Different order should produce different keys
	key2 := relationshipKey(child, parent)
	assert.NotEqual(t, key, key2)
}
