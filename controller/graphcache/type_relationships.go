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

	// confidence tracks how many times a relationship has been observed
	// Key format: "parentGVK|childGVK"
	confidence map[string]int

	// Minimum confidence level to consider a relationship "learned"
	minConfidence int
}

// NewTypeRelationshipCache creates a new cache and seeds it with well-known Kubernetes relationships
func NewTypeRelationshipCache() *TypeRelationshipCache {
	cache := &TypeRelationshipCache{
		descendants:   make(map[schema.GroupVersionKind][]schema.GroupVersionKind),
		confidence:    make(map[string]int),
		minConfidence: 1, // Consider a relationship learned after observing it once
	}

	// Seed with well-known Kubernetes relationships
	cache.seedWellKnownRelationships()

	return cache
}

// seedWellKnownRelationships populates the cache with common Kubernetes resource relationships
func (c *TypeRelationshipCache) seedWellKnownRelationships() {
	// These are well-established patterns in Kubernetes that we can rely on
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

	// Add all well-known relationships with high confidence
	for _, rel := range wellKnownRelationships {
		c.LearnRelationship(rel.Parent, rel.Child, 100) // High confidence for well-known patterns
	}
}

// LearnRelationship records or updates a parent-child relationship
func (c *TypeRelationshipCache) LearnRelationship(parent, child schema.GroupVersionKind, confidence int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update descendants list
	if _, exists := c.descendants[parent]; !exists {
		c.descendants[parent] = []schema.GroupVersionKind{child}
	} else {
		// Check if child already exists in the list
		found := false
		for _, existing := range c.descendants[parent] {
			if existing == child {
				found = true
				break
			}
		}
		if !found {
			c.descendants[parent] = append(c.descendants[parent], child)
		}
	}

	// Update confidence
	key := relationshipKey(parent, child)
	c.confidence[key] += confidence
}

// GetDescendants returns all known child types for a parent type
func (c *TypeRelationshipCache) GetDescendants(parent schema.GroupVersionKind) []schema.GroupVersionKind {
	c.mu.RLock()
	defer c.mu.RUnlock()

	descendants := c.descendants[parent]
	if descendants == nil {
		return []schema.GroupVersionKind{}
	}

	// Return a copy to avoid concurrent modification
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
		return // Avoid cycles
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

// GetConfidence returns the confidence level for a specific relationship
func (c *TypeRelationshipCache) GetConfidence(parent, child schema.GroupVersionKind) int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := relationshipKey(parent, child)
	return c.confidence[key]
}

// GetAllRelationships returns all learned relationships with their confidence levels
func (c *TypeRelationshipCache) GetAllRelationships() map[TypeRelationship]int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make(map[TypeRelationship]int)
	for parent, children := range c.descendants {
		for _, child := range children {
			rel := TypeRelationship{Parent: parent, Child: child}
			key := relationshipKey(parent, child)
			result[rel] = c.confidence[key]
		}
	}
	return result
}

// relationshipKey creates a unique key for a parent-child relationship
func relationshipKey(parent, child schema.GroupVersionKind) string {
	return parent.String() + "|" + child.String()
}

// HasDescendants returns true if the parent type has any known descendants
func (c *TypeRelationshipCache) HasDescendants(parent schema.GroupVersionKind) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	descendants, exists := c.descendants[parent]
	return exists && len(descendants) > 0
}
