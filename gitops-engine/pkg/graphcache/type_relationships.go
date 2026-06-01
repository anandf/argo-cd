package graphcache

import (
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TypeRelationship represents a parent-child relationship between resource types
type TypeRelationship struct {
	Parent schema.GroupVersionKind
	Child  schema.GroupVersionKind
}

// TypeRelationshipCache maintains learned relationships between resource types
// It tracks which resource types typically create which child types (e.g., Deployment -> ReplicaSet)
type TypeRelationshipCache struct {
	mu sync.RWMutex

	// descendants maps parent GVK to list of child GVKs
	descendants map[schema.GroupVersionKind][]schema.GroupVersionKind
}

// NewTypeRelationshipCache creates a new cache and seeds it with well-known Kubernetes relationships
func NewTypeRelationshipCache() *TypeRelationshipCache {
	cache := &TypeRelationshipCache{
		descendants: make(map[schema.GroupVersionKind][]schema.GroupVersionKind),
	}

	cache.seedWellKnownRelationships()

	return cache
}

// seedWellKnownRelationships populates the cache with common Kubernetes resource relationships
func (c *TypeRelationshipCache) seedWellKnownRelationships() {
	wellKnownRelationships := []TypeRelationship{
		// Workload Controllers
		{
			Parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			Child:  schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
		},
		{
			Parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		},
		{
			Parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"},
			Child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		},
		{
			Parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"},
			Child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		},
		{
			Parent: schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"},
			Child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		},
		{
			Parent: schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"},
			Child:  schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"},
		},

		// StatefulSet creates PVCs
		{
			Parent: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"},
			Child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
		},

		// Service discovery
		{
			Parent: schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"},
			Child:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Endpoints"},
		},
		{
			Parent: schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"},
			Child:  schema.GroupVersionKind{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSlice"},
		},
	}

	for _, rel := range wellKnownRelationships {
		c.LearnRelationship(rel.Parent, rel.Child)
	}
}

// LearnRelationship records a parent-child relationship
func (c *TypeRelationshipCache) LearnRelationship(parent, child schema.GroupVersionKind) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.descendants[parent]; !exists {
		c.descendants[parent] = []schema.GroupVersionKind{child}
		return
	}

	for _, existing := range c.descendants[parent] {
		if existing == child {
			return
		}
	}
	c.descendants[parent] = append(c.descendants[parent], child)
}

// GetDescendants returns all known child types for a parent type
func (c *TypeRelationshipCache) GetDescendants(parent schema.GroupVersionKind) []schema.GroupVersionKind {
	c.mu.RLock()
	defer c.mu.RUnlock()

	descendants := c.descendants[parent]
	if descendants == nil {
		return []schema.GroupVersionKind{}
	}

	result := make([]schema.GroupVersionKind, len(descendants))
	copy(result, descendants)
	return result
}

// GetAllDescendantsRecursive returns all descendants recursively (children, grandchildren, etc.)
func (c *TypeRelationshipCache) GetAllDescendantsRecursive(parent schema.GroupVersionKind) []schema.GroupVersionKind {
	c.mu.RLock()
	defer c.mu.RUnlock()

	visited := make(map[schema.GroupVersionKind]bool)
	result := []schema.GroupVersionKind{}

	c.collectDescendantsRecursive(parent, visited, &result)

	return result
}

// collectDescendantsRecursive is the internal recursive helper
func (c *TypeRelationshipCache) collectDescendantsRecursive(parent schema.GroupVersionKind, visited map[schema.GroupVersionKind]bool, result *[]schema.GroupVersionKind) {
	if visited[parent] {
		return
	}
	visited[parent] = true

	descendants := c.descendants[parent]
	for _, child := range descendants {
		if !visited[child] {
			*result = append(*result, child)
			c.collectDescendantsRecursive(child, visited, result)
		}
	}
}

// GetAllLearnedRelationships returns all parent-child pairs for persistence
func (c *TypeRelationshipCache) GetAllLearnedRelationships() []TypeRelationship {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var result []TypeRelationship
	for parent, children := range c.descendants {
		for _, child := range children {
			result = append(result, TypeRelationship{Parent: parent, Child: child})
		}
	}
	return result
}

// HasDescendants returns true if the parent type has any known descendants
func (c *TypeRelationshipCache) HasDescendants(parent schema.GroupVersionKind) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	descendants, exists := c.descendants[parent]
	return exists && len(descendants) > 0
}
