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

func newTestPersistence(t *testing.T) (*RelationshipPersistenceManager, *TypeRelationshipCache, *fake.Clientset) {
	t.Helper()
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()
	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
	})
	manager := NewRelationshipPersistenceManager(PersistenceConfig{
		Store: store,
		Cache: cache,
	})
	return manager, cache, kubeClient
}

func TestPersistenceManager_SaveCreatesConfigMap(t *testing.T) {
	manager, cache, kubeClient := newTestPersistence(t)

	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Foo"},
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Bar"},
		5,
	)

	err := manager.Save()
	require.NoError(t, err)

	cm, err := kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	require.NoError(t, err)
	assert.NotNil(t, cm)

	assert.Equal(t, "argocd-graph-cache", cm.Labels["app.kubernetes.io/name"])
	assert.Equal(t, "relationship-cache", cm.Labels["app.kubernetes.io/component"])

	data, ok := cm.Data[RelationshipDataKey]
	assert.True(t, ok)
	assert.NotEmpty(t, data)

	var persisted PersistedRelationships
	err = json.Unmarshal([]byte(data), &persisted)
	require.NoError(t, err)
	assert.Equal(t, "v1", persisted.Version)
	assert.True(t, len(persisted.Relationships) > 0)
}

func TestPersistenceManager_SaveUpdatesExistingConfigMap(t *testing.T) {
	manager, cache, kubeClient := newTestPersistence(t)

	err := manager.Save()
	require.NoError(t, err)

	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"},
		3,
	)

	time.Sleep(10 * time.Millisecond)
	err = manager.Save()
	require.NoError(t, err)

	cm, err := kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	require.NoError(t, err)

	var persisted PersistedRelationships
	err = json.Unmarshal([]byte(cm.Data[RelationshipDataKey]), &persisted)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(persisted.Relationships), 10)
}

func TestPersistenceManager_StartLoadsNoConfigMap(t *testing.T) {
	manager, _, _ := newTestPersistence(t)

	err := manager.Start()
	assert.NoError(t, err)
}

func TestPersistenceManager_StartLoadsRelationships(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	// First: save some relationships with cache1
	cache1 := NewTypeRelationshipCache()
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

	store1 := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd"})
	mgr1 := NewRelationshipPersistenceManager(PersistenceConfig{Store: store1, Cache: cache1})
	err := mgr1.Save()
	require.NoError(t, err)

	// Second: create new cache (simulating restart) and load
	cache2 := NewTypeRelationshipCache()
	initialCount := len(cache2.GetAllRelationships())

	store2 := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd"})
	mgr2 := NewRelationshipPersistenceManager(PersistenceConfig{Store: store2, Cache: cache2})
	err = mgr2.Start()
	require.NoError(t, err)

	allRels := cache2.GetAllRelationships()
	assert.Greater(t, len(allRels), initialCount)

	fooGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Foo"}
	barGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Bar"}
	descendants := cache2.GetDescendants(fooGVK)
	assert.Contains(t, descendants, barGVK)

	confidence := cache2.GetConfidence(fooGVK, barGVK)
	assert.Equal(t, 10, confidence)
}

func TestPersistenceManager_SaveFiltersLowConfidence(t *testing.T) {
	manager, cache, kubeClient := newTestPersistence(t)

	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Low"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Confidence"},
		1, // Below MinConfidenceToPersist (2)
	)
	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "High"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Confidence"},
		5,
	)

	err := manager.Save()
	require.NoError(t, err)

	cm, err := kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	require.NoError(t, err)

	var persisted PersistedRelationships
	err = json.Unmarshal([]byte(cm.Data[RelationshipDataKey]), &persisted)
	require.NoError(t, err)

	for _, rel := range persisted.Relationships {
		if rel.Parent == "test.io/v1/Low" {
			t.Error("Low confidence relationship should not be persisted")
		}
	}
}

func TestPersistenceManager_SaveIncludesMetadata(t *testing.T) {
	manager, cache, kubeClient := newTestPersistence(t)

	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Parent"},
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Child"},
		3,
	)

	err := manager.Save()
	require.NoError(t, err)

	cm, err := kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	require.NoError(t, err)

	var persisted PersistedRelationships
	err = json.Unmarshal([]byte(cm.Data[RelationshipDataKey]), &persisted)
	require.NoError(t, err)

	assert.Greater(t, persisted.Metadata.TotalRelationships, 0)
	assert.Greater(t, persisted.Metadata.SeededCount, 0)
	assert.Greater(t, persisted.Metadata.LearnedCount, 0)
}

func TestPersistenceManager_GetStats(t *testing.T) {
	manager, _, _ := newTestPersistence(t)

	stats := manager.GetStats()
	assert.Equal(t, int64(0), stats["persistence_count"])

	err := manager.Save()
	require.NoError(t, err)

	stats = manager.GetStats()
	assert.Equal(t, int64(1), stats["persistence_count"])
	assert.Equal(t, "configmap", stats["store_type"])
	assert.Equal(t, "argocd", stats["store_namespace"])
}

func TestPersistenceManager_StopSavesOnShutdown(t *testing.T) {
	manager, cache, kubeClient := newTestPersistence(t)

	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "A"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "B"},
		5,
	)

	err := manager.Start()
	require.NoError(t, err)

	err = manager.Stop()
	require.NoError(t, err)

	_, err = kubeClient.CoreV1().ConfigMaps("argocd").Get(
		context.Background(),
		RelationshipConfigMapName,
		metav1.GetOptions{},
	)
	assert.NoError(t, err)
}

func TestPersistenceManager_StartWithPreExistingConfigMap(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

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

	cache := NewTypeRelationshipCache()
	store := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd"})
	manager := NewRelationshipPersistenceManager(PersistenceConfig{Store: store, Cache: cache})

	err = manager.Start()
	require.NoError(t, err)

	parentGVK := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	childGVK := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}
	descendants := cache.GetDescendants(parentGVK)
	assert.Contains(t, descendants, childGVK)
}

func TestConfigMapStore_LoadHandlesCorruptedData(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

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

	store := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd"})
	_, err = store.Load()
	assert.Error(t, err)
}

func TestPersistenceManager_StartSkipsInvalidGVK(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

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
	store := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd"})
	manager := NewRelationshipPersistenceManager(PersistenceConfig{Store: store, Cache: cache})

	err = manager.Start()
	assert.NoError(t, err)

	allRels := cache.GetAllRelationships()
	for rel := range allRels {
		assert.NotEqual(t, "invalid-gvk-format", rel.Parent.String())
	}
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
		},
		{
			name:  "apps group resource",
			input: "apps/v1/Deployment",
			expected: schema.GroupVersionKind{
				Group:   "apps",
				Version: "v1",
				Kind:    "Deployment",
			},
		},
		{
			name:  "Custom resource",
			input: "custom.io/v1alpha1/MyResource",
			expected: schema.GroupVersionKind{
				Group:   "custom.io",
				Version: "v1alpha1",
				Kind:    "MyResource",
			},
		},
		{
			name:    "Invalid format",
			input:   "invalid",
			wantErr: true,
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

func TestGVKToString(t *testing.T) {
	tests := []struct {
		name     string
		gvk      schema.GroupVersionKind
		expected string
	}{
		{
			name:     "Core resource",
			gvk:      schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
			expected: "/v1/Pod",
		},
		{
			name:     "Apps group",
			gvk:      schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			expected: "apps/v1/Deployment",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, gvkToString(tc.gvk))
		})
	}
}

func TestGVKStringRoundTrip(t *testing.T) {
	gvks := []schema.GroupVersionKind{
		{Group: "", Version: "v1", Kind: "Pod"},
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "custom.io", Version: "v1alpha1", Kind: "MyResource"},
	}

	for _, gvk := range gvks {
		str := gvkToString(gvk)
		parsed, err := parseGVKString(str)
		require.NoError(t, err)
		assert.Equal(t, gvk, parsed, "round-trip failed for %s", str)
	}
}
