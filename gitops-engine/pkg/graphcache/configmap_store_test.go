package graphcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2/textlogger"
)

var testLog = textlogger.NewLogger(textlogger.NewConfig())

func TestConfigMapStore_SaveAndLoad(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
		Log:        testLog,
	})

	relationships := []PersistedRelationship{
		{
			Parent: "apps/v1/Deployment",
			Child:  "apps/v1/ReplicaSet",
		},
		{
			Parent: "custom.io/v1/Foo",
			Child:  "custom.io/v1/Bar",
		},
	}

	metadata := PersistedRelationshipMetadata{
		TotalRelationships: 2,
		SeededCount:        1,
		LearnedCount:       1,
	}

	err := store.Save(relationships, metadata)
	require.NoError(t, err)

	loaded, err := store.Load()
	require.NoError(t, err)

	assert.Len(t, loaded, 2)
	assert.Equal(t, relationships[0].Parent, loaded[0].Parent)
	assert.Equal(t, relationships[0].Child, loaded[0].Child)
}

func TestConfigMapStore_IntegrationWithCache(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	cache := NewTypeRelationshipCache()

	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"},
	)

	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
		Log:        testLog,
	})

	allRels := cache.GetAllLearnedRelationships()
	var relationships []PersistedRelationship
	for _, rel := range allRels {
		relationships = append(relationships, PersistedRelationship{
			Parent: GvkToString(rel.Parent),
			Child:  GvkToString(rel.Child),
		})
	}

	err := store.Save(relationships, PersistedRelationshipMetadata{
		TotalRelationships: len(relationships),
	})
	require.NoError(t, err)

	// Simulate restart: new cache, load from same store
	cache2 := NewTypeRelationshipCache()
	store2 := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
		Log:        testLog,
	})

	loaded, err := store2.Load()
	require.NoError(t, err)

	for _, rel := range loaded {
		parentGVK, err := ParseGVKString(rel.Parent)
		if err != nil {
			continue
		}
		childGVK, err := ParseGVKString(rel.Child)
		if err != nil {
			continue
		}
		cache2.LearnRelationship(parentGVK, childGVK)
	}

	parentGVK := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"}
	childGVK := schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"}

	descendants := cache2.GetDescendants(parentGVK)
	assert.Contains(t, descendants, childGVK)
}

func TestConfigMapStore_LoadEmpty(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
		Log:        testLog,
	})

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded)
}

func TestConfigMapStore_CloseIsNoop(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
		Log:        testLog,
	})

	err := store.Close()
	assert.NoError(t, err)
}
