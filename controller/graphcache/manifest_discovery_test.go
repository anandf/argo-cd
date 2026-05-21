package graphcache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

func TestNewManifestDiscovery(t *testing.T) {
	typeRelationships := NewTypeRelationshipCache()

	config := ManifestDiscoveryConfig{
		RepoServerClient:  nil, // Not testing actual repo server calls
		TypeRelationships: typeRelationships,
		GraphCache:        nil,
		CacheTTL:          5 * time.Minute,
	}

	md := NewManifestDiscovery(config)

	assert.NotNil(t, md)
	assert.NotNil(t, md.manifestCache)
	assert.Equal(t, 5*time.Minute, md.cacheTTL)
}

func TestNewManifestDiscovery_DefaultTTL(t *testing.T) {
	config := ManifestDiscoveryConfig{
		TypeRelationships: NewTypeRelationshipCache(),
	}

	md := NewManifestDiscovery(config)

	assert.Equal(t, 5*time.Minute, md.cacheTTL, "Should use default TTL")
}

func TestExpandWithDescendants(t *testing.T) {
	typeRelationships := NewTypeRelationshipCache()
	md := &ManifestDiscovery{
		typeRelationships: typeRelationships,
	}

	// Test with Deployment -> should expand to ReplicaSet and Pod
	deployment := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	expanded := md.expandWithDescendants([]schema.GroupVersionKind{deployment})

	// Should contain Deployment, ReplicaSet, and Pod
	assert.Contains(t, expanded, deployment)
	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"})
	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"})
}

func TestExpandWithDescendants_MultipleTypes(t *testing.T) {
	typeRelationships := NewTypeRelationshipCache()
	md := &ManifestDiscovery{
		typeRelationships: typeRelationships,
	}

	gvks := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "", Version: "v1", Kind: "Service"},
	}

	expanded := md.expandWithDescendants(gvks)

	// Should contain originals
	assert.Contains(t, expanded, gvks[0])
	assert.Contains(t, expanded, gvks[1])

	// Should contain Deployment descendants
	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"})
	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"})

	// Should contain Service descendants
	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Endpoints"})
}

func TestCacheGVKs_AndGetCached(t *testing.T) {
	md := &ManifestDiscovery{
		manifestCache: make(map[string]*manifestCacheEntry),
		cacheTTL:      5 * time.Minute,
	}

	app := &appv1.Application{}
	app.Name = "test-app"
	app.Namespace = "argocd"
	app.Status.Sync.Revision = "abc123"

	gvks := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "", Version: "v1", Kind: "Service"},
	}

	// Cache the GVKs
	md.cacheGVKs(app, gvks)

	// Retrieve from cache
	cached := md.getCachedGVKs(app)

	assert.NotNil(t, cached)
	assert.Len(t, cached, 2)
	assert.Contains(t, cached, gvks[0])
	assert.Contains(t, cached, gvks[1])
}

func TestGetCachedGVKs_Expired(t *testing.T) {
	md := &ManifestDiscovery{
		manifestCache: make(map[string]*manifestCacheEntry),
		cacheTTL:      1 * time.Millisecond, // Very short TTL
	}

	app := &appv1.Application{}
	app.Name = "test-app"
	app.Namespace = "argocd"

	gvks := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
	}

	// Cache the GVKs
	md.cacheGVKs(app, gvks)

	// Wait for cache to expire
	time.Sleep(2 * time.Millisecond)

	// Should return nil because cache expired
	cached := md.getCachedGVKs(app)
	assert.Nil(t, cached)
}

func TestGetCachedGVKs_CommitSHAChanged(t *testing.T) {
	md := &ManifestDiscovery{
		manifestCache: make(map[string]*manifestCacheEntry),
		cacheTTL:      5 * time.Minute,
	}

	app := &appv1.Application{}
	app.Name = "test-app"
	app.Namespace = "argocd"
	app.Status.Sync.Revision = "abc123"

	gvks := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
	}

	// Cache the GVKs
	md.cacheGVKs(app, gvks)

	// Change the commit SHA
	app.Status.Sync.Revision = "def456"

	// Should return nil because source changed
	cached := md.getCachedGVKs(app)
	assert.Nil(t, cached)
}

func TestInvalidateCache(t *testing.T) {
	md := &ManifestDiscovery{
		manifestCache: make(map[string]*manifestCacheEntry),
		cacheTTL:      5 * time.Minute,
	}

	app := &appv1.Application{}
	app.Name = "test-app"
	app.Namespace = "argocd"

	gvks := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
	}

	// Cache the GVKs
	md.cacheGVKs(app, gvks)

	// Verify it's cached
	cached := md.getCachedGVKs(app)
	assert.NotNil(t, cached)

	// Invalidate the cache
	md.InvalidateCache(app.Namespace, app.Name)

	// Should no longer be cached
	cached = md.getCachedGVKs(app)
	assert.Nil(t, cached)
}

func TestClearExpiredCache(t *testing.T) {
	md := &ManifestDiscovery{
		manifestCache: make(map[string]*manifestCacheEntry),
		cacheTTL:      1 * time.Millisecond,
	}

	// Add some entries
	app1 := &appv1.Application{}
	app1.Name = "app1"
	app1.Namespace = "argocd"

	app2 := &appv1.Application{}
	app2.Name = "app2"
	app2.Namespace = "argocd"

	gvks := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
	}

	md.cacheGVKs(app1, gvks)
	md.cacheGVKs(app2, gvks)

	assert.Len(t, md.manifestCache, 2)

	// Wait for cache to expire
	time.Sleep(2 * time.Millisecond)

	// Clear expired entries
	md.ClearExpiredCache()

	// All entries should be removed
	assert.Len(t, md.manifestCache, 0)
}

func TestGetCacheStats(t *testing.T) {
	md := &ManifestDiscovery{
		manifestCache: make(map[string]*manifestCacheEntry),
		cacheTTL:      5 * time.Minute,
	}

	// Add some entries
	for i := 0; i < 3; i++ {
		app := &appv1.Application{}
		app.Name = "app" + string(rune('1'+i))
		app.Namespace = "argocd"

		gvks := []schema.GroupVersionKind{
			{Group: "apps", Version: "v1", Kind: "Deployment"},
		}

		md.cacheGVKs(app, gvks)
	}

	stats := md.GetCacheStats()

	assert.Equal(t, 3, stats["total_entries"])
	assert.Equal(t, "5m0s", stats["cache_ttl"])
	assert.Equal(t, 0, stats["expired_entries"])
}

func TestExtractPodSpec_Deployment(t *testing.T) {
	md := &ManifestDiscovery{}

	deployment := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"image": "nginx:1.14",
							},
						},
					},
				},
			},
		},
	}

	podSpec := md.extractPodSpec(deployment)

	assert.NotNil(t, podSpec)
	containers, found, _ := unstructured.NestedSlice(podSpec, "containers")
	assert.True(t, found)
	assert.Len(t, containers, 1)
}

func TestExtractPodSpec_Pod(t *testing.T) {
	md := &ManifestDiscovery{}

	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "nginx",
						"image": "nginx:1.14",
					},
				},
			},
		},
	}

	podSpec := md.extractPodSpec(pod)

	assert.NotNil(t, podSpec)
	containers, found, _ := unstructured.NestedSlice(podSpec, "containers")
	assert.True(t, found)
	assert.Len(t, containers, 1)
}

func TestExtractReferencedResources_ConfigMapVolume(t *testing.T) {
	md := &ManifestDiscovery{}

	deployment := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"volumes": []interface{}{
							map[string]interface{}{
								"name": "config",
								"configMap": map[string]interface{}{
									"name": "my-config",
								},
							},
						},
						"containers": []interface{}{
							map[string]interface{}{
								"name": "app",
							},
						},
					},
				},
			},
		},
	}

	gvks := make(map[schema.GroupVersionKind]bool)
	md.extractReferencedResources(deployment, gvks)

	configMapGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	assert.True(t, gvks[configMapGVK], "Should detect ConfigMap reference")
}

func TestExtractReferencedResources_SecretVolume(t *testing.T) {
	md := &ManifestDiscovery{}

	deployment := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"volumes": []interface{}{
							map[string]interface{}{
								"name": "secret-vol",
								"secret": map[string]interface{}{
									"secretName": "my-secret",
								},
							},
						},
						"containers": []interface{}{
							map[string]interface{}{
								"name": "app",
							},
						},
					},
				},
			},
		},
	}

	gvks := make(map[schema.GroupVersionKind]bool)
	md.extractReferencedResources(deployment, gvks)

	secretGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}
	assert.True(t, gvks[secretGVK], "Should detect Secret reference")
}

func TestExtractReferencedResources_EnvFromConfigMap(t *testing.T) {
	md := &ManifestDiscovery{}

	deployment := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name": "app",
								"envFrom": []interface{}{
									map[string]interface{}{
										"configMapRef": map[string]interface{}{
											"name": "my-config",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	gvks := make(map[schema.GroupVersionKind]bool)
	md.extractReferencedResources(deployment, gvks)

	configMapGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	assert.True(t, gvks[configMapGVK], "Should detect ConfigMap reference in envFrom")
}

func TestExtractReferencedResources_EnvValueFromSecret(t *testing.T) {
	md := &ManifestDiscovery{}

	deployment := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name": "app",
								"env": []interface{}{
									map[string]interface{}{
										"name": "PASSWORD",
										"valueFrom": map[string]interface{}{
											"secretKeyRef": map[string]interface{}{
												"name": "my-secret",
												"key":  "password",
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	gvks := make(map[schema.GroupVersionKind]bool)
	md.extractReferencedResources(deployment, gvks)

	secretGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}
	assert.True(t, gvks[secretGVK], "Should detect Secret reference in env valueFrom")
}

