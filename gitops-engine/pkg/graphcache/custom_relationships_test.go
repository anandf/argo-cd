package graphcache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2/textlogger"
)

var testCRLog = textlogger.NewLogger(textlogger.NewConfig())

func TestLoadCustomRelationships_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.yaml")

	content := `relationships:
  - parent:
      group: "stable.example.com"
      version: "v1"
      kind: "CronTab"
    child:
      group: "stable.example.com"
      version: "v1"
      kind: "CronTabSchedule"
  - parent:
      group: "argoproj.io"
      version: "v1alpha1"
      kind: "Rollout"
    child:
      group: "apps"
      version: "v1"
      kind: "ReplicaSet"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	rels, err := LoadCustomRelationships(testCRLog, path)
	require.NoError(t, err)
	assert.Len(t, rels, 2)

	assert.Equal(t, schema.GroupVersionKind{Group: "stable.example.com", Version: "v1", Kind: "CronTab"}, rels[0].Parent)
	assert.Equal(t, schema.GroupVersionKind{Group: "stable.example.com", Version: "v1", Kind: "CronTabSchedule"}, rels[0].Child)

	assert.Equal(t, schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Rollout"}, rels[1].Parent)
	assert.Equal(t, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, rels[1].Child)
}

func TestLoadCustomRelationships_MissingFile(t *testing.T) {
	rels, err := LoadCustomRelationships(testCRLog, "/nonexistent/path/relationships.yaml")
	assert.NoError(t, err)
	assert.Nil(t, rels)
}

func TestLoadCustomRelationships_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.yaml")

	require.NoError(t, os.WriteFile(path, []byte(""), 0o644))

	rels, err := LoadCustomRelationships(testCRLog, path)
	require.NoError(t, err)
	assert.Empty(t, rels)
}

func TestLoadCustomRelationships_EmptyRelationshipsList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.yaml")

	require.NoError(t, os.WriteFile(path, []byte("relationships: []\n"), 0o644))

	rels, err := LoadCustomRelationships(testCRLog, path)
	require.NoError(t, err)
	assert.Empty(t, rels)
}

func TestLoadCustomRelationships_MalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.yaml")

	require.NoError(t, os.WriteFile(path, []byte("{{invalid yaml"), 0o644))

	_, err := LoadCustomRelationships(testCRLog, path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse")
}

func TestLoadCustomRelationships_MissingKind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.yaml")

	content := `relationships:
  - parent:
      group: "test.io"
      version: "v1"
      kind: "Parent"
    child:
      group: "test.io"
      version: "v1"
  - parent:
      group: "valid.io"
      version: "v1"
      kind: "Good"
    child:
      group: "valid.io"
      version: "v1"
      kind: "AlsoGood"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	rels, err := LoadCustomRelationships(testCRLog, path)
	require.NoError(t, err)
	assert.Len(t, rels, 1, "Should skip entry with missing child kind")
	assert.Equal(t, "Good", rels[0].Parent.Kind)
}

func TestLoadCustomRelationships_MissingVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.yaml")

	content := `relationships:
  - parent:
      group: "test.io"
      kind: "NoVersion"
    child:
      group: "test.io"
      version: "v1"
      kind: "Child"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	rels, err := LoadCustomRelationships(testCRLog, path)
	require.NoError(t, err)
	assert.Empty(t, rels, "Should skip entry with missing parent version")
}

func TestLoadCustomRelationships_CoreGroupResource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.yaml")

	content := `relationships:
  - parent:
      group: ""
      version: "v1"
      kind: "Service"
    child:
      group: ""
      version: "v1"
      kind: "Endpoints"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	rels, err := LoadCustomRelationships(testCRLog, path)
	require.NoError(t, err)
	assert.Len(t, rels, 1)
	assert.Equal(t, "", rels[0].Parent.Group)
	assert.Equal(t, "Service", rels[0].Parent.Kind)
}

func TestLoadCustomRelationships_IntegrationWithCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationships.yaml")

	content := `relationships:
  - parent:
      group: "custom.io"
      version: "v1"
      kind: "Operator"
    child:
      group: "custom.io"
      version: "v1"
      kind: "ManagedResource"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	rels, err := LoadCustomRelationships(testCRLog, path)
	require.NoError(t, err)

	cache := NewTypeRelationshipCache()
	for _, rel := range rels {
		cache.LearnRelationship(rel.Parent, rel.Child)
	}

	parentGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Operator"}
	childGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "ManagedResource"}

	descendants := cache.GetDescendants(parentGVK)
	assert.Contains(t, descendants, childGVK)
}
