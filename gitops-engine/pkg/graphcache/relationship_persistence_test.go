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
	"k8s.io/klog/v2/textlogger"
)

var testRPLog = textlogger.NewLogger(textlogger.NewConfig())

func TestConfigMapStore_SaveAndLoad_Relationships(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
		Log:        testRPLog,
	})

	cache := NewTypeRelationshipCache()
	cache.LearnRelationship(
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Foo"},
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Bar"},
	)

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

func TestConfigMapStore_UpdateExisting(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
		Log:        testRPLog,
	})

	err := store.Save([]PersistedRelationship{}, PersistedRelationshipMetadata{})
	require.NoError(t, err)

	relationships := []PersistedRelationship{
		{Parent: "test.io/v1/Parent", Child: "test.io/v1/Child"},
	}

	time.Sleep(10 * time.Millisecond)
	err = store.Save(relationships, PersistedRelationshipMetadata{
		TotalRelationships: 1,
		LearnedCount:       1,
	})
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
	assert.Len(t, persisted.Relationships, 1)
}

func TestConfigMapStore_LoadReturnsEmptyWhenNotFound(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	store := NewConfigMapStore(ConfigMapStoreConfig{
		KubeClient: kubeClient,
		Namespace:  "argocd",
		Log:        testRPLog,
	})

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded)
}

func TestConfigMapStore_LoadAndRestoreToCache(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	// Save relationships
	store1 := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd", Log: testRPLog})

	cache1 := NewTypeRelationshipCache()
	cache1.LearnRelationship(
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Foo"},
		schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Bar"},
	)
	cache1.LearnRelationship(
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Parent"},
		schema.GroupVersionKind{Group: "test.io", Version: "v1", Kind: "Child"},
	)

	allRels := cache1.GetAllLearnedRelationships()
	var relationships []PersistedRelationship
	for _, rel := range allRels {
		relationships = append(relationships, PersistedRelationship{
			Parent: GvkToString(rel.Parent),
			Child:  GvkToString(rel.Child),
		})
	}
	err := store1.Save(relationships, PersistedRelationshipMetadata{
		TotalRelationships: len(relationships),
	})
	require.NoError(t, err)

	// Load into new cache (simulating restart)
	store2 := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd", Log: testRPLog})
	loaded, err := store2.Load()
	require.NoError(t, err)

	cache2 := NewTypeRelationshipCache()
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

	fooGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Foo"}
	barGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Bar"}
	descendants := cache2.GetDescendants(fooGVK)
	assert.Contains(t, descendants, barGVK)
}

func TestConfigMapStore_LoadWithPreExistingConfigMap(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	relationships := PersistedRelationships{
		Version:     "v1",
		LastUpdated: time.Now(),
		Relationships: []PersistedRelationship{
			{
				Parent: "test.io/v1/Parent",
				Child:  "test.io/v1/Child",
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

	store := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd", Log: testRPLog})
	loaded, err := store.Load()
	require.NoError(t, err)

	assert.Len(t, loaded, 1)
	assert.Equal(t, "test.io/v1/Parent", loaded[0].Parent)
	assert.Equal(t, "test.io/v1/Child", loaded[0].Child)
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

	store := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd", Log: testRPLog})
	_, err = store.Load()
	assert.Error(t, err)
}

func TestConfigMapStore_LoadSkipsInvalidGVK(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	relationships := PersistedRelationships{
		Version:     "v1",
		LastUpdated: time.Now(),
		Relationships: []PersistedRelationship{
			{
				Parent: "invalid-gvk-format",
				Child:  "test.io/v1/Child",
			},
			{
				Parent: "test.io/v1/ValidParent",
				Child:  "test.io/v1/ValidChild",
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

	store := NewConfigMapStore(ConfigMapStoreConfig{KubeClient: kubeClient, Namespace: "argocd", Log: testRPLog})
	loaded, err := store.Load()
	require.NoError(t, err)

	// Both are returned from Load — the caller filters invalid GVKs
	assert.Len(t, loaded, 2)

	// Verify that parsing the invalid one fails
	_, parseErr := ParseGVKString(loaded[0].Parent)
	assert.Error(t, parseErr)
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
			result, err := ParseGVKString(tc.input)
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
			assert.Equal(t, tc.expected, GvkToString(tc.gvk))
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
		str := GvkToString(gvk)
		parsed, err := ParseGVKString(str)
		require.NoError(t, err)
		assert.Equal(t, gvk, parsed, "round-trip failed for %s", str)
	}
}

