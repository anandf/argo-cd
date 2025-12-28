package graphcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func TestConfigMapStore_SaveAndLoad(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
	})

	// Create some test relationships
	relationships := []PersistedRelationship{
		{
			Parent:     "apps/v1/Deployment",
			Child:      "apps/v1/ReplicaSet",
			Confidence: 100,
		},
		{
			Parent:     "custom.io/v1/Foo",
			Child:      "custom.io/v1/Bar",
			Confidence: 10,
		},
	}

	metadata := PersistedRelationshipMetadata{
		TotalRelationships: 2,
		SeededCount:        1,
		LearnedCount:       1,
	}

	// Save
	err := store.Save(relationships, metadata)
	require.NoError(t, err)

	// Load
	loaded, err := store.Load()
	require.NoError(t, err)

	assert.Len(t, loaded, 2)
	assert.Equal(t, relationships[0].Parent, loaded[0].Parent)
	assert.Equal(t, relationships[0].Child, loaded[0].Child)
	assert.Equal(t, relationships[0].Confidence, loaded[0].Confidence)
}

func TestConfigMapStore_IntegrationWithManager(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	// Add some relationships to cache
	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"},
		5,
	)

	// Create store
	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
	})

	// Create manager with the store
	manager := NewRelationshipPersistenceManager(PersistenceConfig{
		Store:         store,
		Cache:         cache,
		MinConfidence: 2,
	})

	// Save
	err := manager.Save()
	require.NoError(t, err)

	// Create new cache and manager (simulating restart)
	cache2 := NewTypeRelationshipCache()
	store2 := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
	})
	manager2 := NewRelationshipPersistenceManager(PersistenceConfig{
		Store: store2,
		Cache: cache2,
	})

	// Load
	err = manager2.Start()
	require.NoError(t, err)

	// Verify relationship was loaded
	parentGVK := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	childGVK := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}

	descendants := cache2.GetDescendants(parentGVK)
	assert.Contains(t, descendants, childGVK)
	assert.Equal(t, 5, cache2.GetConfidence(parentGVK, childGVK))
}

func TestConfigMapStore_GetStats(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "test-ns",
	})

	stats := store.GetStats()

	assert.Equal(t, "configmap", stats["type"])
	assert.Equal(t, "test-ns", stats["namespace"])
	assert.Equal(t, RelationshipConfigMapName, stats["configmap"])
}

func TestConfigMapStore_LoadEmpty(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
	})

	// Load when no ConfigMap exists
	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded)
}

func TestConfigMapStore_Close(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
	})

	// Close should not error
	err := store.Close()
	assert.NoError(t, err)
}
