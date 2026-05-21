package graphcache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

// RedisStore implements RelationshipStore using Redis
// This is an example implementation showing how to add Redis persistence
type RedisStore struct {
	client *redis.Client
	key    string
	ttl    time.Duration

	mu        sync.Mutex
	lastSaved time.Time
	saveCount int64
}

// RedisStoreConfig configures the Redis store
type RedisStoreConfig struct {
	// Client is the Redis client
	Client *redis.Client

	// Key is the Redis key to store relationships (e.g., "argocd:graph-cache:relationships")
	Key string

	// TTL is the time-to-live for the key (0 = no expiration)
	TTL time.Duration
}

// NewRedisStore creates a new Redis-based relationship store
func NewRedisStore(config RedisStoreConfig) *RedisStore {
	if config.Key == "" {
		config.Key = "argocd:graph-cache:relationships"
	}

	return &RedisStore{
		client: config.Client,
		key:    config.Key,
		ttl:    config.TTL,
	}
}

// Save persists relationships to Redis
func (s *RedisStore) Save(relationships []PersistedRelationship, metadata PersistedRelationshipMetadata) error {
	// Marshal outside the lock — this is pure computation, no shared state.
	persisted := PersistedRelationships{
		Version:       "v1",
		LastUpdated:   time.Now(),
		Relationships: relationships,
		Metadata:      metadata,
	}

	data, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("failed to marshal relationships: %w", err)
	}

	// Network call outside the lock.
	ctx := context.Background()
	if err := s.client.Set(ctx, s.key, data, s.ttl).Err(); err != nil {
		return fmt.Errorf("failed to save to redis: %w", err)
	}

	// Only hold the lock for bookkeeping.
	s.mu.Lock()
	s.lastSaved = time.Now()
	s.saveCount++
	saveCount := s.saveCount
	s.mu.Unlock()

	log.WithFields(log.Fields{
		"component":  "graph-cache",
		"store":      "redis",
		"key":        s.key,
		"size_bytes": len(data),
		"save_count": saveCount,
	}).Debug("Saved relationships to Redis")

	return nil
}

// Load retrieves relationships from Redis
func (s *RedisStore) Load() ([]PersistedRelationship, error) {
	// Network call outside the lock.
	ctx := context.Background()
	data, err := s.client.Get(ctx, s.key).Bytes()
	if err != nil {
		if err == redis.Nil {
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"store":     "redis",
				"key":       s.key,
			}).Info("No persisted relationships found in Redis")
			return []PersistedRelationship{}, nil
		}
		return nil, fmt.Errorf("failed to get from redis: %w", err)
	}

	var persisted PersistedRelationships
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, fmt.Errorf("failed to unmarshal relationships: %w", err)
	}

	log.WithFields(log.Fields{
		"component":    "graph-cache",
		"store":        "redis",
		"key":          s.key,
		"loaded":       len(persisted.Relationships),
		"last_updated": persisted.LastUpdated,
		"seeded":       persisted.Metadata.SeededCount,
		"learned":      persisted.Metadata.LearnedCount,
	}).Info("Loaded relationships from Redis")

	return persisted.Relationships, nil
}

// SaveSnapshot persists the graph snapshot for a specific cluster
func (s *RedisStore) SaveSnapshot(clusterServer string, snapshot *GraphSnapshot) error {
	key := fmt.Sprintf("%s:snapshot:%s", s.key, clusterServer)
	
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	ctx := context.Background()
	if err := s.client.Set(ctx, key, data, s.ttl).Err(); err != nil {
		return fmt.Errorf("failed to save snapshot to redis: %w", err)
	}
	
	log.WithFields(log.Fields{
		"component":  "graph-cache",
		"store":      "redis",
		"cluster":    clusterServer,
		"size_bytes": len(data),
		"nodes":      len(snapshot.Nodes),
	}).Info("Saved graph snapshot to Redis")
	
	return nil
}

// LoadSnapshot retrieves the graph snapshot for a specific cluster
func (s *RedisStore) LoadSnapshot(clusterServer string) (*GraphSnapshot, error) {
	key := fmt.Sprintf("%s:snapshot:%s", s.key, clusterServer)
	ctx := context.Background()
	
	data, err := s.client.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, nil // Not found is not an error
		}
		return nil, fmt.Errorf("failed to get snapshot from redis: %w", err)
	}

	var snapshot GraphSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("failed to unmarshal snapshot: %w", err)
	}
	
	log.WithFields(log.Fields{
		"component": "graph-cache",
		"store":     "redis",
		"cluster":   clusterServer,
		"nodes":     len(snapshot.Nodes),
		"age":       time.Since(snapshot.LastUpdated),
	}).Info("Loaded graph snapshot from Redis")

	return &snapshot, nil
}

// Close performs cleanup (closes Redis client)
func (s *RedisStore) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

// GetStats returns statistics about the Redis store
func (s *RedisStore) GetStats() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := map[string]interface{}{
		"type":       "redis",
		"key":        s.key,
		"last_saved": s.lastSaved,
		"save_count": s.saveCount,
	}

	if !s.lastSaved.IsZero() {
		stats["time_since_last_save"] = time.Since(s.lastSaved).String()
	}

	// Get Redis info
	ctx := context.Background()
	if info, err := s.client.Info(ctx, "memory").Result(); err == nil {
		stats["redis_info"] = info
	}

	return stats
}

// Example usage:
//
//   import "github.com/redis/go-redis/v9"
//
//   redisClient := redis.NewClient(&redis.Options{
//       Addr: "localhost:6379",
//   })
//
//   store := NewRedisStore(RedisStoreConfig{
//       Client: redisClient,
//       Key:    "argocd:graph-cache:relationships",
//   })
//
//   manager := NewRelationshipPersistenceManager(PersistenceConfig{
//       Store: store,
//       Cache: typeRelationshipCache,
//       AutoSave: true,
//   })
//
//   manager.Start()
//   defer manager.Stop()
