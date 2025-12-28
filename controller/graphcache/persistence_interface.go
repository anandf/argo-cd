package graphcache

import (
	log "github.com/sirupsen/logrus"
)

// RelationshipStore is the interface for persisting learned resource relationships.
// Implementations can use ConfigMap, Redis, etcd, databases, or any other storage backend.
type RelationshipStore interface {
	// Save persists all relationships with confidence >= minConfidence
	Save(relationships []PersistedRelationship, metadata PersistedRelationshipMetadata) error

	// Load retrieves all persisted relationships
	Load() ([]PersistedRelationship, error)

	// Close performs cleanup and final save
	Close() error

	// GetStats returns statistics about the storage backend
	GetStats() map[string]interface{}
}

// RelationshipPersistenceManager manages the lifecycle of relationship persistence
// It works with any RelationshipStore implementation
type RelationshipPersistenceManager struct {
	store             RelationshipStore
	cache             *TypeRelationshipCache
	minConfidence     int
	persistenceCount  int64
	autoSaveEnabled   bool
}

// PersistenceConfig contains configuration for relationship persistence
type PersistenceConfig struct {
	// Store is the backend storage implementation
	Store RelationshipStore

	// Cache is the TypeRelationshipCache to persist
	Cache *TypeRelationshipCache

	// MinConfidence is the minimum confidence to persist (default: 2)
	MinConfidence int

	// AutoSave enables automatic background persistence
	// If false, caller must manually call Save()
	AutoSave bool
}

// NewRelationshipPersistenceManager creates a new persistence manager
// with the provided store implementation
func NewRelationshipPersistenceManager(config PersistenceConfig) *RelationshipPersistenceManager {
	if config.MinConfidence == 0 {
		config.MinConfidence = MinConfidenceToPersist
	}

	return &RelationshipPersistenceManager{
		store:           config.Store,
		cache:           config.Cache,
		minConfidence:   config.MinConfidence,
		autoSaveEnabled: config.AutoSave,
	}
}

// Start loads existing relationships and optionally starts background persistence
func (m *RelationshipPersistenceManager) Start() error {
	// Load existing relationships from store
	relationships, err := m.store.Load()
	if err != nil {
		return err
	}

	// Load into cache
	for _, rel := range relationships {
		parentGVK, err := parseGVKString(rel.Parent)
		if err != nil {
			log.WithError(err).WithField("parent", rel.Parent).Warn("Failed to parse parent GVK")
			continue
		}

		childGVK, err := parseGVKString(rel.Child)
		if err != nil {
			log.WithError(err).WithField("child", rel.Child).Warn("Failed to parse child GVK")
			continue
		}

		m.cache.LearnRelationship(parentGVK, childGVK, rel.Confidence)
	}

	log.WithFields(log.Fields{
		"component": "graph-cache",
		"loaded":    len(relationships),
	}).Info("Loaded persisted relationships")

	return nil
}

// Save persists current relationships to the store
func (m *RelationshipPersistenceManager) Save() error {
	allRels := m.cache.GetAllRelationships()

	// Filter by minimum confidence
	relationships := []PersistedRelationship{}
	seededCount := 0
	learnedCount := 0

	for rel, confidence := range allRels {
		if confidence < m.minConfidence {
			continue
		}

		relationships = append(relationships, PersistedRelationship{
			Parent:     gvkToString(rel.Parent),
			Child:      gvkToString(rel.Child),
			Confidence: confidence,
		})

		if confidence >= 50 {
			seededCount++
		} else {
			learnedCount++
		}
	}

	metadata := PersistedRelationshipMetadata{
		TotalRelationships: len(relationships),
		SeededCount:        seededCount,
		LearnedCount:       learnedCount,
	}

	if err := m.store.Save(relationships, metadata); err != nil {
		return err
	}

	m.persistenceCount++

	log.WithFields(log.Fields{
		"component":         "graph-cache",
		"relationships":     len(relationships),
		"seeded":            seededCount,
		"learned":           learnedCount,
		"persistence_count": m.persistenceCount,
	}).Info("Saved relationships")

	return nil
}

// Stop performs final save and cleanup
func (m *RelationshipPersistenceManager) Stop() error {
	// Final save
	if err := m.Save(); err != nil {
		log.WithError(err).Warn("Failed to save relationships on shutdown")
	}

	// Close the store
	return m.store.Close()
}

// GetStats returns statistics from both manager and store
func (m *RelationshipPersistenceManager) GetStats() map[string]interface{} {
	stats := map[string]interface{}{
		"persistence_count": m.persistenceCount,
		"min_confidence":    m.minConfidence,
		"auto_save":         m.autoSaveEnabled,
	}

	// Merge store stats
	storeStats := m.store.GetStats()
	for k, v := range storeStats {
		stats["store_"+k] = v
	}

	return stats
}

// Example usage with different stores:
//
// ConfigMap Store:
//   store := NewConfigMapStore(kubeClient, "argocd")
//   manager := NewRelationshipPersistenceManager(PersistenceConfig{
//       Store: store,
//       Cache: typeRelationshipCache,
//       AutoSave: true,
//   })
//
// Redis Store (future):
//   store := NewRedisStore(redisClient, "argocd:relationships")
//   manager := NewRelationshipPersistenceManager(PersistenceConfig{
//       Store: store,
//       Cache: typeRelationshipCache,
//   })
//
// Custom Store:
//   type MyStore struct { ... }
//   func (s *MyStore) Save(...) error { ... }
//   func (s *MyStore) Load() ([]PersistedRelationship, error) { ... }
//   func (s *MyStore) Close() error { ... }
//   func (s *MyStore) GetStats() map[string]interface{} { ... }
//
//   store := &MyStore{}
//   manager := NewRelationshipPersistenceManager(PersistenceConfig{Store: store, Cache: cache})
