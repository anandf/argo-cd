package graphcache

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// ConfigMap name for storing learned relationships
	RelationshipConfigMapName = "argocd-graph-cache-relationships"

	// ConfigMap key for the relationships data
	RelationshipDataKey = "relationships.json"

	// How often to persist relationships to ConfigMap
	DefaultPersistInterval = 5 * time.Minute
)

// PersistedRelationships represents the JSON structure stored in ConfigMap
type PersistedRelationships struct {
	Version       string                        `json:"version"`
	LastUpdated   time.Time                     `json:"lastUpdated"`
	Relationships []PersistedRelationship       `json:"relationships"`
	Metadata      PersistedRelationshipMetadata `json:"metadata"`
}

// PersistedRelationship represents a single parent-child relationship
type PersistedRelationship struct {
	Parent string `json:"parent"` // GVK string: "group/version/kind"
	Child  string `json:"child"`  // GVK string: "group/version/kind"
}

// PersistedRelationshipMetadata contains metadata about the persisted data
type PersistedRelationshipMetadata struct {
	TotalRelationships int    `json:"totalRelationships"`
	SeededCount        int    `json:"seededCount"`
	LearnedCount       int    `json:"learnedCount"`
	ControllerVersion  string `json:"controllerVersion,omitempty"`
}

// GvkToString converts a GVK to string format "group/version/kind"
func GvkToString(gvk schema.GroupVersionKind) string {
	return fmt.Sprintf("%s/%s/%s", gvk.Group, gvk.Version, gvk.Kind)
}

// ParseGVKString parses a GVK string in format "group/version/kind"
// Handles core group (empty group): "/v1/Pod" or "apps/v1/Deployment"
func ParseGVKString(s string) (schema.GroupVersionKind, error) {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 3 {
		return schema.GroupVersionKind{}, fmt.Errorf("invalid GVK format: %s", s)
	}

	return schema.GroupVersionKind{
		Group:   parts[0],
		Version: parts[1],
		Kind:    parts[2],
	}, nil
}
