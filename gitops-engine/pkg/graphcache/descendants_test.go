package graphcache

import (
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNewDescendantTracker(t *testing.T) {
	dt := NewDescendantTracker()
	require.NotNil(t, dt)
	assert.NotEmpty(t, dt.implicitRelationships)
}

func TestGetExpectedDescendants_Deployment(t *testing.T) {
	dt := NewDescendantTracker()
	descendants := dt.GetExpectedDescendants(schema.GroupKind{Group: "apps", Kind: "Deployment"})
	assert.Contains(t, descendants, schema.GroupKind{Group: "apps", Kind: "ReplicaSet"})
}

func TestGetExpectedDescendants_ReplicaSet(t *testing.T) {
	dt := NewDescendantTracker()
	descendants := dt.GetExpectedDescendants(schema.GroupKind{Group: "apps", Kind: "ReplicaSet"})
	assert.Contains(t, descendants, schema.GroupKind{Group: "", Kind: "Pod"})
}

func TestGetExpectedDescendants_StatefulSet(t *testing.T) {
	dt := NewDescendantTracker()
	descendants := dt.GetExpectedDescendants(schema.GroupKind{Group: "apps", Kind: "StatefulSet"})
	assert.Contains(t, descendants, schema.GroupKind{Group: "", Kind: "Pod"})
	assert.Contains(t, descendants, schema.GroupKind{Group: "", Kind: "PersistentVolumeClaim"})
}

func TestGetExpectedDescendants_Service(t *testing.T) {
	dt := NewDescendantTracker()
	descendants := dt.GetExpectedDescendants(schema.GroupKind{Group: "", Kind: "Service"})
	assert.Contains(t, descendants, schema.GroupKind{Group: "", Kind: "Endpoints"})
	assert.Contains(t, descendants, schema.GroupKind{Group: "discovery.k8s.io", Kind: "EndpointSlice"})
}

func TestGetExpectedDescendants_CronJob(t *testing.T) {
	dt := NewDescendantTracker()
	descendants := dt.GetExpectedDescendants(schema.GroupKind{Group: "batch", Kind: "CronJob"})
	assert.Contains(t, descendants, schema.GroupKind{Group: "batch", Kind: "Job"})
}

func TestGetExpectedDescendants_Unknown(t *testing.T) {
	dt := NewDescendantTracker()
	descendants := dt.GetExpectedDescendants(schema.GroupKind{Group: "custom.io", Kind: "Foo"})
	assert.Empty(t, descendants)
}

func TestExtractParentReferences_OwnerRef(t *testing.T) {
	dt := NewDescendantTracker()

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "nginx-abc123",
				"namespace": "default",
				"uid":       "pod-uid",
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"apiVersion": "apps/v1",
						"kind":       "ReplicaSet",
						"name":       "nginx-abc",
						"uid":        "rs-uid",
					},
				},
			},
		},
	}

	parents := dt.ExtractParentReferences(obj)
	require.Len(t, parents, 1)
	assert.Equal(t, "ReplicaSet", parents[0].ResourceKey.Kind)
	assert.Equal(t, "apps", parents[0].ResourceKey.Group)
	assert.Equal(t, "nginx-abc", parents[0].ResourceKey.Name)
	assert.Equal(t, "default", parents[0].ResourceKey.Namespace)
	assert.Equal(t, "rs-uid", parents[0].UID)
}

func TestExtractParentReferences_Endpoints(t *testing.T) {
	dt := NewDescendantTracker()

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Endpoints",
			"metadata": map[string]interface{}{
				"name":      "my-service",
				"namespace": "default",
			},
		},
	}

	parents := dt.ExtractParentReferences(obj)
	found := false
	for _, p := range parents {
		if p.ResourceKey.Kind == "Service" && p.ResourceKey.Name == "my-service" {
			found = true
		}
	}
	assert.True(t, found, "should find implicit Service parent")
}

func TestExtractParentReferences_EndpointSlice(t *testing.T) {
	dt := NewDescendantTracker()

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "discovery.k8s.io/v1",
			"kind":       "EndpointSlice",
			"metadata": map[string]interface{}{
				"name":      "my-service-abc",
				"namespace": "default",
				"labels": map[string]interface{}{
					"kubernetes.io/service-name": "my-service",
				},
			},
		},
	}

	parents := dt.ExtractParentReferences(obj)
	found := false
	for _, p := range parents {
		if p.ResourceKey.Kind == "Service" && p.ResourceKey.Name == "my-service" {
			found = true
		}
	}
	assert.True(t, found, "should find Service parent via label")
}

func TestExtractParentReferences_SecretTokenSA(t *testing.T) {
	dt := NewDescendantTracker()

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":      "my-sa-token-abc",
				"namespace": "default",
				"annotations": map[string]interface{}{
					"kubernetes.io/service-account.name": "my-sa",
				},
			},
		},
	}

	parents := dt.ExtractParentReferences(obj)
	found := false
	for _, p := range parents {
		if p.ResourceKey.Kind == "ServiceAccount" && p.ResourceKey.Name == "my-sa" {
			found = true
		}
	}
	assert.True(t, found, "should find ServiceAccount parent via annotation")
}

func TestExtractGroup(t *testing.T) {
	assert.Equal(t, "apps", extractGroup("apps/v1"))
	assert.Equal(t, "", extractGroup("v1"))
	assert.Equal(t, "batch", extractGroup("batch/v1"))
}

func TestExtractStatefulSetName(t *testing.T) {
	assert.Equal(t, "web", extractStatefulSetName("web-0"))
	assert.Equal(t, "my-sts", extractStatefulSetName("my-sts-3"))
	assert.Equal(t, "", extractStatefulSetName("nopod"))
	assert.Equal(t, "", extractStatefulSetName("not-ordinal-abc"))
}

func TestBuildOwnerReference(t *testing.T) {
	node := &ResourceNode{
		Key: kube.ResourceKey{
			Group:     "apps",
			Kind:      "ReplicaSet",
			Namespace: "default",
			Name:      "nginx-abc",
		},
		Version: "v1",
		UID:     "rs-uid-123",
	}

	ref := BuildOwnerReference(node)
	assert.Equal(t, "apps/v1", ref.APIVersion)
	assert.Equal(t, "ReplicaSet", ref.Kind)
	assert.Equal(t, "nginx-abc", ref.Name)
	assert.Equal(t, "rs-uid-123", string(ref.UID))
}

func TestBuildOwnerReference_CoreGroup(t *testing.T) {
	node := &ResourceNode{
		Key: kube.ResourceKey{
			Group:     "",
			Kind:      "Service",
			Namespace: "default",
			Name:      "my-svc",
		},
		Version: "v1",
		UID:     "svc-uid",
	}

	ref := BuildOwnerReference(node)
	assert.Equal(t, "v1", ref.APIVersion)
}

func TestBuildOwnerReference_DefaultVersion(t *testing.T) {
	node := &ResourceNode{
		Key: kube.ResourceKey{
			Group: "",
			Kind:  "Pod",
			Name:  "test",
		},
		UID: "pod-uid",
	}

	ref := BuildOwnerReference(node)
	assert.Equal(t, "v1", ref.APIVersion)
}

func TestResourceReference_String(t *testing.T) {
	ref := ResourceReference{
		GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
		Namespace: "default",
		Name:      "nginx",
		RefType:   RefTypeVolume,
	}
	assert.Equal(t, "apps/Deployment:default/nginx", ref.String())

	coreRef := ResourceReference{
		GroupKind: schema.GroupKind{Group: "", Kind: "Secret"},
		Namespace: "default",
		Name:      "my-secret",
		RefType:   RefTypeEnv,
	}
	assert.Equal(t, "core/Secret:default/my-secret", coreRef.String())
}

func TestResourceReference_ToResourceKey(t *testing.T) {
	ref := ResourceReference{
		GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
		Namespace: "default",
		Name:      "nginx",
	}

	key := ref.ToResourceKey()
	assert.Equal(t, "apps", key.Group)
	assert.Equal(t, "Deployment", key.Kind)
	assert.Equal(t, "default", key.Namespace)
	assert.Equal(t, "nginx", key.Name)
}

func TestExtractReferencedResources_Pod(t *testing.T) {
	dt := NewDescendantTracker()

	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"volumes": []interface{}{
					map[string]interface{}{
						"name": "secret-vol",
						"secret": map[string]interface{}{
							"secretName": "my-secret",
						},
					},
					map[string]interface{}{
						"name": "cm-vol",
						"configMap": map[string]interface{}{
							"name": "my-configmap",
						},
					},
				},
				"containers": []interface{}{
					map[string]interface{}{
						"name": "app",
						"envFrom": []interface{}{
							map[string]interface{}{
								"secretRef": map[string]interface{}{
									"name": "env-secret",
								},
							},
						},
					},
				},
			},
		},
	}

	refs := dt.ExtractReferencedResources(pod)
	assert.GreaterOrEqual(t, len(refs), 3)

	names := make(map[string]bool)
	for _, ref := range refs {
		names[ref.Name] = true
	}
	assert.True(t, names["my-secret"])
	assert.True(t, names["my-configmap"])
	assert.True(t, names["env-secret"])
}

func TestExtractReferencedResources_Deployment(t *testing.T) {
	dt := NewDescendantTracker()

	deploy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "test-deploy",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"volumes": []interface{}{
							map[string]interface{}{
								"name": "config",
								"configMap": map[string]interface{}{
									"name": "app-config",
								},
							},
						},
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx",
							},
						},
					},
				},
			},
		},
	}

	refs := dt.ExtractReferencedResources(deploy)
	assert.GreaterOrEqual(t, len(refs), 1)

	found := false
	for _, ref := range refs {
		if ref.Name == "app-config" && ref.GroupKind.Kind == "ConfigMap" {
			found = true
		}
	}
	assert.True(t, found, "should find ConfigMap reference from Deployment's pod template")
}
