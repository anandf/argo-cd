package graphcache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2/textlogger"
)

var testFSLog = textlogger.NewLogger(textlogger.NewConfig())

func TestNewFileStore_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "nested", "dir")
	path := filepath.Join(subDir, "relationships.json")

	store, err := NewFileStore(FileStoreConfig{FilePath: path, Log: testFSLog})
	require.NoError(t, err)
	assert.NotNil(t, store)

	info, err := os.Stat(subDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestNewFileStore_EmptyPath(t *testing.T) {
	_, err := NewFileStore(FileStoreConfig{FilePath: "", Log: testFSLog})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "file path is required")
}

func TestFileStore_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.json")

	store, err := NewFileStore(FileStoreConfig{FilePath: path, Log: testFSLog})
	require.NoError(t, err)

	relationships := []PersistedRelationship{
		{Parent: "apps/v1/Deployment", Child: "apps/v1/ReplicaSet"},
		{Parent: "custom.io/v1/Foo", Child: "custom.io/v1/Bar"},
	}
	metadata := PersistedRelationshipMetadata{
		TotalRelationships: 2,
		SeededCount:        1,
		LearnedCount:       1,
	}

	err = store.Save(relationships, metadata)
	require.NoError(t, err)

	loaded, err := store.Load()
	require.NoError(t, err)

	assert.Len(t, loaded, 2)
	assert.Equal(t, "apps/v1/Deployment", loaded[0].Parent)
	assert.Equal(t, "apps/v1/ReplicaSet", loaded[0].Child)
	assert.Equal(t, "custom.io/v1/Foo", loaded[1].Parent)
	assert.Equal(t, "custom.io/v1/Bar", loaded[1].Child)
}

func TestFileStore_LoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	store, err := NewFileStore(FileStoreConfig{FilePath: path, Log: testFSLog})
	require.NoError(t, err)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded)
}

func TestFileStore_LoadCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.json")

	require.NoError(t, os.WriteFile(path, []byte("invalid json{]"), 0o644))

	store, err := NewFileStore(FileStoreConfig{FilePath: path, Log: testFSLog})
	require.NoError(t, err)

	_, err = store.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal")
}

func TestFileStore_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.json")

	store, err := NewFileStore(FileStoreConfig{FilePath: path, Log: testFSLog})
	require.NoError(t, err)

	// First save
	err = store.Save([]PersistedRelationship{
		{Parent: "apps/v1/Deployment", Child: "apps/v1/ReplicaSet"},
	}, PersistedRelationshipMetadata{TotalRelationships: 1})
	require.NoError(t, err)

	// Second save (overwrites)
	err = store.Save([]PersistedRelationship{
		{Parent: "batch/v1/Job", Child: "/v1/Pod"},
	}, PersistedRelationshipMetadata{TotalRelationships: 1})
	require.NoError(t, err)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Len(t, loaded, 1)
	assert.Equal(t, "batch/v1/Job", loaded[0].Parent)

	// No temp files left behind
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "Only the relationships.json file should exist")
}

func TestFileStore_SavePreservesFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.json")

	store, err := NewFileStore(FileStoreConfig{FilePath: path, Log: testFSLog})
	require.NoError(t, err)

	err = store.Save([]PersistedRelationship{
		{Parent: "/v1/Pod", Child: "/v1/Pod"},
	}, PersistedRelationshipMetadata{TotalRelationships: 1})
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var persisted PersistedRelationships
	require.NoError(t, json.Unmarshal(data, &persisted))
	assert.Equal(t, "v1", persisted.Version)
	assert.Len(t, persisted.Relationships, 1)
	assert.Equal(t, 1, persisted.Metadata.TotalRelationships)
}

func TestFileStore_Close(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.json")

	store, err := NewFileStore(FileStoreConfig{FilePath: path, Log: testFSLog})
	require.NoError(t, err)

	err = store.Close()
	assert.NoError(t, err)
}
