package graphcache

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNewRelationshipPersistence(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	assert.NotNil(t, persistence)
	assert.Equal(t, "argocd", persistence.namespace)
	assert.Equal(t, RelationshipConfigMapName, persistence.configMapName)
	assert.Equal(t, DefaultPersistInterval, persistence.persistInterval)
}

func TestSave_CreatesConfigMap(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	// Add some learned relationships
	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Foo"},
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Bar"},
		5,
	)

	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	// Save relationships
	err := persistence.Save()
	require.NoError(t, err)

	// Verify ConfigMap was created
	cm, err := kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	require.NoError(t, err)
	assert.NotNil(t, cm)

	// Verify labels
	assert.Equal(t, "argocd-graph-cache", cm.Labels["app.kubernetes.io/name"])
	assert.Equal(t, "relationship-cache", cm.Labels["app.kubernetes.io/component"])

	// Verify data exists
	data, ok := cm.Data[RelationshipDataKey]
	assert.True(t, ok)
	assert.NotEmpty(t, data)

	// Parse and verify content
	var persisted PersistedRelationships
	err = json.Unmarshal([]byte(data), &persisted)
	require.NoError(t, err)

	assert.Equal(t, "v1", persisted.Version)
	assert.True(t, len(persisted.Relationships) > 0)
}

func TestSave_UpdatesExistingConfigMap(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	// First save
	err := persistence.Save()
	require.NoError(t, err)

	// Get the ConfigMap
	_, err = kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	require.NoError(t, err)

	// Add more relationships
	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"},
		3,
	)

	// Second save
	time.Sleep(10 * time.Millisecond) // Ensure different timestamp
	err = persistence.Save()
	require.NoError(t, err)

	// Get the updated ConfigMap
	cm2, err := kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	require.NoError(t, err)

	// Verify the data was updated by checking content
	var persisted PersistedRelationships
	err = json.Unmarshal([]byte(cm2.Data[RelationshipDataKey]), &persisted)
	require.NoError(t, err)
	assert.Greater(t, len(persisted.Relationships), 10) // Should have more than just seeded
}

func TestLoad_NoConfigMap(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	// Load should not error when ConfigMap doesn't exist
	err := persistence.Load()
	assert.NoError(t, err)
}

func TestLoad_LoadsRelationships(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache1 := NewTypeRelationshipCache()

	// Add some relationships
	cache1.LearnRelationship(
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Foo"},
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Bar"},
		10,
	)
	cache1.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"},
		5,
	)

	persistence1 := NewRelationshipPersistence(kubeClient, "argocd", cache1)

	// Save relationships
	err := persistence1.Save()
	require.NoError(t, err)

	// Create a new cache (simulating restart)
	cache2 := NewTypeRelationshipCache()

	// Before loading, cache2 should only have seeded relationships
	allRels := cache2.GetAllRelationships()
	initialCount := len(allRels)

	persistence2 := NewRelationshipPersistence(kubeClient, "argocd", cache2)

	// Load relationships
	err = persistence2.Load()
	require.NoError(t, err)

	// After loading, cache2 should have more relationships
	allRels = cache2.GetAllRelationships()
	assert.Greater(t, len(allRels), initialCount)

	// Verify specific relationships were loaded
	fooGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Foo"}
	barGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Bar"}

	descendants := cache2.GetDescendants(fooGVK)
	assert.Contains(t, descendants, barGVK)

	// Verify confidence was restored
	confidence := cache2.GetConfidence(fooGVK, barGVK)
	assert.Equal(t, 10, confidence)
}

func TestSave_FiltersLowConfidence(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	// Add relationship with confidence below minimum
	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Low"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Confidence"},
		1, // Below MinConfidenceToPersist (2)
	)

	// Add relationship with confidence above minimum
	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "High"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Confidence"},
		5,
	)

	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	// Save relationships
	err := persistence.Save()
	require.NoError(t, err)

	// Load the ConfigMap
	cm, err := kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	require.NoError(t, err)

	var persisted PersistedRelationships
	err = json.Unmarshal([]byte(cm.Data[RelationshipDataKey]), &persisted)
	require.NoError(t, err)

	// Low confidence relationship should not be persisted
	for _, rel := range persisted.Relationships {
		if rel.Parent == "test.io/v1/Low" {
			t.Error("Low confidence relationship should not be persisted")
		}
	}
}

func TestSave_IncludesMetadata(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	// Add a learned relationship
	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Parent"},
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Child"},
		3,
	)

	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	// Save relationships
	err := persistence.Save()
	require.NoError(t, err)

	// Load and verify metadata
	cm, err := kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	require.NoError(t, err)

	var persisted PersistedRelationships
	err = json.Unmarshal([]byte(cm.Data[RelationshipDataKey]), &persisted)
	require.NoError(t, err)

	// Verify metadata
	assert.Greater(t, persisted.Metadata.TotalRelationships, 0)
	assert.Greater(t, persisted.Metadata.SeededCount, 0) // Well-known patterns
	assert.Greater(t, persisted.Metadata.LearnedCount, 0) // Our custom relationship
}

func TestGetStats(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	// Before any persistence
	stats := persistence.GetStats()
	assert.Equal(t, int64(0), stats["persistence_count"])

	// After save
	err := persistence.Save()
	require.NoError(t, err)

	stats = persistence.GetStats()
	assert.Equal(t, int64(1), stats["persistence_count"])
	assert.NotNil(t, stats["last_persisted"])
	assert.Equal(t, "argocd", stats["namespace"])
	assert.Equal(t, RelationshipConfigMapName, stats["config_map_name"])
}

func TestParseGVKString(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected schema.GroupVersionKind
		wantErr  bool
	}{
		{
			name:  "Core resource (Pod)",
			input: "/v1/Pod",
			expected: schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "Pod",
			},
			wantErr: false,
		},
		{
			name:  "apps group resource",
			input: "apps/v1/Deployment",
			expected: schema.GroupVersionKind{
				Group:   "apps",
				Version: "v1",
				Kind:    "Deployment",
			},
			wantErr: false,
		},
		{
			name:  "Custom resource",
			input: "custom.io/v1alpha1/MyResource",
			expected: schema.GroupVersionKind{
				Group:   "custom.io",
				Version: "v1alpha1",
				Kind:    "MyResource",
			},
			wantErr: false,
		},
		{
			name:     "Invalid format",
			input:    "invalid",
			expected: schema.GroupVersionKind{},
			wantErr:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseGVKString(tc.input)

			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestStart_LoadsOnStartup(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	// Create a ConfigMap with some relationships
	relationships := PersistedRelationships{
		Version:     "v1",
		LastUpdated: time.Now(),
		Relationships: []PersistedRelationship{
			{
				Parent:     "test.io/v1/Parent",
				Child:      "test.io/v1/Child",
				Confidence: 10,
			},
		},
		Metadata: PersistedRelationshipMetadata{
			TotalRelationships: 1,
			LearnedCount:       1,
		},
	}

	data, err := json.Marshal(relationships)
	require.NoError(t, err)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RelationshipConfigMapName,
			Namespace: "argocd",
		},
		Data: map[string]string{
			RelationshipDataKey: string(data),
		},
	}

	_, err = kubeClient.CoreV1().ConfigMaps("argocd").Create(
		context.Background(),
		cm,
		metav1.CreateOptions{},
	)
	require.NoError(t, err)

	// Create persistence (should load on Start)
	cache := NewTypeRelationshipCache()
	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	err = persistence.Start()
	require.NoError(t, err)
	defer persistence.Stop()

	// Verify relationship was loaded
	parentGVK := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	childGVK := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}

	descendants := cache.GetDescendants(parentGVK)
	assert.Contains(t, descendants, childGVK)
}

func TestStop_SavesOnShutdown(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	// Add a relationship
	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "A"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "B"},
		5,
	)

	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	err := persistence.Start()
	require.NoError(t, err)

	// Stop should save
	err = persistence.Stop()
	require.NoError(t, err)

	// Verify ConfigMap exists
	_, err = kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	assert.NoError(t, err)
}

func TestLoad_HandlesCorruptedData(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	// Create ConfigMap with invalid JSON
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RelationshipConfigMapName,
			Namespace: "argocd",
		},
		Data: map[string]string{
			RelationshipDataKey: "invalid json{]",
		},
	}

	_, err := kubeClient.CoreV1().ConfigMaps("argocd").Create(
		context.Background(),
		cm,
		metav1.CreateOptions{},
	)
	require.NoError(t, err)

	cache := NewTypeRelationshipCache()
	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	// Load should return error but not crash
	err = persistence.Load()
	assert.Error(t, err)
}

func TestLoad_HandlesInvalidGVK(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	// Create ConfigMap with invalid GVK format
	relationships := PersistedRelationships{
		Version:     "v1",
		LastUpdated: time.Now(),
		Relationships: []PersistedRelationship{
			{
				Parent:     "invalid-gvk-format",
				Child:      "test.io/v1/Child",
				Confidence: 10,
			},
		},
	}

	data, err := json.Marshal(relationships)
	require.NoError(t, err)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RelationshipConfigMapName,
			Namespace: "argocd",
		},
		Data: map[string]string{
			RelationshipDataKey: string(data),
		},
	}

	_, err = kubeClient.CoreV1().ConfigMaps("argocd").Create(
		context.Background(),
		cm,
		metav1.CreateOptions{},
	)
	require.NoError(t, err)

	cache := NewTypeRelationshipCache()
	persistence := NewRelationshipPersistence(kubeClient, "argocd", cache)

	// Load should not error, but should skip invalid entry
	err = persistence.Load()
	assert.NoError(t, err)

	// Cache should only have seeded relationships (invalid one skipped)
	allRels := cache.GetAllRelationships()
	for rel := range allRels {
		// Should not have the invalid relationship
		assert.NotEqual(t, "invalid-gvk-format", rel.Parent.String())
	}
}
