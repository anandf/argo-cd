package graphcache

// RelationshipStore is the interface for persisting learned resource relationships.
// Implementations can use ConfigMap, Redis, etcd, databases, or any other storage backend.
type RelationshipStore interface {
	// Save persists all relationships
	Save(relationships []PersistedRelationship, metadata PersistedRelationshipMetadata) error

	// Load retrieves all persisted relationships
	Load() ([]PersistedRelationship, error)

	// Close performs cleanup
	Close() error
}
