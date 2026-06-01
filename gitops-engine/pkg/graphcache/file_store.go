package graphcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// FileStore implements RelationshipStore using a local JSON file.
type FileStore struct {
	filePath  string
	log       logr.Logger
	mu        sync.Mutex
	lastSaved time.Time
	saveCount int64
}

// FileStoreConfig configures the file-based relationship store.
type FileStoreConfig struct {
	FilePath string
	Log      logr.Logger
}

// NewFileStore creates a new file-based relationship store.
// It creates the parent directory if it does not exist.
func NewFileStore(config FileStoreConfig) (*FileStore, error) {
	if config.FilePath == "" {
		return nil, fmt.Errorf("file path is required")
	}

	dir := filepath.Dir(config.FilePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	return &FileStore{
		filePath: config.FilePath,
		log:      config.Log,
	}, nil
}

// Save persists relationships to a JSON file using atomic write (temp file + rename).
func (s *FileStore) Save(relationships []PersistedRelationship, metadata PersistedRelationshipMetadata) error {
	persisted := PersistedRelationships{
		Version:       "v1",
		LastUpdated:   time.Now(),
		Relationships: relationships,
		Metadata:      metadata,
	}

	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal relationships: %w", err)
	}

	dir := filepath.Dir(s.filePath)
	tmp, err := os.CreateTemp(dir, ".relationships-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.filePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	s.mu.Lock()
	s.lastSaved = time.Now()
	s.saveCount++
	s.mu.Unlock()

	s.log.V(1).Info("Saved relationships to file", "path", s.filePath, "size_bytes", len(data))

	return nil
}

// Load retrieves relationships from the JSON file.
// Returns an empty slice and nil error if the file does not exist.
func (s *FileStore) Load() ([]PersistedRelationship, error) {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			s.log.Info("No persisted relationships file found")
			return []PersistedRelationship{}, nil
		}
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var persisted PersistedRelationships
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, fmt.Errorf("failed to unmarshal relationships: %w", err)
	}

	s.log.Info("Loaded relationships from file",
		"loaded", len(persisted.Relationships), "last_updated", persisted.LastUpdated)

	return persisted.Relationships, nil
}

// Close performs cleanup (no-op for file store).
func (s *FileStore) Close() error {
	return nil
}
