package graphcache

import (
	"testing"

	graphcore "github.com/argoproj/argo-cd/gitops-engine/pkg/graphcache"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNewManifestDiscovery(t *testing.T) {
	typeRelationships := graphcore.NewTypeRelationshipCache()

	config := ManifestDiscoveryConfig{
		RepoServerClient:  nil,
		TypeRelationships: typeRelationships,
		GraphCache:        nil,
	}

	md := NewManifestDiscovery(config)

	assert.NotNil(t, md)
	assert.NotNil(t, md.typeRelationships)
}

func TestExpandWithDescendants(t *testing.T) {
	typeRelationships := graphcore.NewTypeRelationshipCache()
	md := &ManifestDiscovery{
		typeRelationships: typeRelationships,
	}

	deployment := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	expanded := md.expandWithDescendants([]schema.GroupVersionKind{deployment})

	assert.Contains(t, expanded, deployment)
	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"})
	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"})
}

func TestExpandWithDescendants_MultipleTypes(t *testing.T) {
	typeRelationships := graphcore.NewTypeRelationshipCache()
	md := &ManifestDiscovery{
		typeRelationships: typeRelationships,
	}

	gvks := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "", Version: "v1", Kind: "Service"},
	}

	expanded := md.expandWithDescendants(gvks)

	assert.Contains(t, expanded, gvks[0])
	assert.Contains(t, expanded, gvks[1])

	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"})
	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"})

	assert.Contains(t, expanded, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Endpoints"})
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
