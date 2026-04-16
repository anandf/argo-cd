package graphcache

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
)

// DescendantTracker handles discovery of resource descendants and parent-child relationships.
type DescendantTracker struct {
	// Explicit relationships (well-known Kubernetes patterns)
	implicitRelationships map[schema.GroupKind][]schema.GroupKind
}

// NewDescendantTracker creates a new descendant tracker with predefined relationship rules.
func NewDescendantTracker() *DescendantTracker {
	dt := &DescendantTracker{
		implicitRelationships: make(map[schema.GroupKind][]schema.GroupKind),
	}

	// Define well-known implicit relationships (resources without OwnerReferences)
	dt.defineImplicitRelationships()

	return dt
}

// defineImplicitRelationships sets up the implicit parent-child relationships.
// These are resources that are related but don't use OwnerReferences.
func (dt *DescendantTracker) defineImplicitRelationships() {
	// Service → Endpoints, EndpointSlice
	dt.addImplicitRelationship(
		schema.GroupKind{Group: "", Kind: "Service"},
		schema.GroupKind{Group: "", Kind: "Endpoints"},
	)
	dt.addImplicitRelationship(
		schema.GroupKind{Group: "", Kind: "Service"},
		schema.GroupKind{Group: "discovery.k8s.io", Kind: "EndpointSlice"},
	)

	// StatefulSet → PersistentVolumeClaim (via volumeClaimTemplates)
	dt.addImplicitRelationship(
		schema.GroupKind{Group: "apps", Kind: "StatefulSet"},
		schema.GroupKind{Group: "", Kind: "PersistentVolumeClaim"},
	)

	// ServiceAccount → Secret (token secrets)
	dt.addImplicitRelationship(
		schema.GroupKind{Group: "", Kind: "ServiceAccount"},
		schema.GroupKind{Group: "", Kind: "Secret"},
	)

	// Note: Pod → Secret and Pod → ConfigMap are not included here because
	// they would cause watching all Secrets/ConfigMaps. Instead, we'll detect
	// these relationships when parsing Pod specs.
}

// addImplicitRelationship adds a parent → child implicit relationship.
func (dt *DescendantTracker) addImplicitRelationship(parent, child schema.GroupKind) {
	dt.implicitRelationships[parent] = append(dt.implicitRelationships[parent], child)
}

// ExtractParentReferences extracts parent resource references from a resource.
// This includes both explicit OwnerReferences and implicit relationships.
func (dt *DescendantTracker) ExtractParentReferences(obj *unstructured.Unstructured) []ParentRef {
	var parents []ParentRef

	// Extract explicit owner references
	ownerRefs := obj.GetOwnerReferences()
	for _, ownerRef := range ownerRefs {
		parentKey := kube.ResourceKey{
			Group:     extractGroup(ownerRef.APIVersion),
			Kind:      ownerRef.Kind,
			Namespace: obj.GetNamespace(), // Same namespace as child
			Name:      ownerRef.Name,
		}
		parents = append(parents, ParentRef{
			ResourceKey: parentKey,
			UID:         string(ownerRef.UID),
		})
	}

	// Extract implicit parent references based on resource type
	implicitParents := dt.extractImplicitParents(obj)
	parents = append(parents, implicitParents...)

	return parents
}

// extractImplicitParents extracts parent references that don't use OwnerReferences.
func (dt *DescendantTracker) extractImplicitParents(obj *unstructured.Unstructured) []ParentRef {
	var parents []ParentRef

	gvk := obj.GroupVersionKind()
	gk := schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}

	switch gk {
	case schema.GroupKind{Group: "", Kind: "Endpoints"}:
		// Endpoints → Service (same name)
		parents = append(parents, ParentRef{
			ResourceKey: kube.ResourceKey{
				Group:     "",
				Kind:      "Service",
				Namespace: obj.GetNamespace(),
				Name:      obj.GetName(), // Same name as endpoints
			},
		})

	case schema.GroupKind{Group: "discovery.k8s.io", Kind: "EndpointSlice"}:
		// EndpointSlice → Service (via kubernetes.io/service-name label)
		labels := obj.GetLabels()
		if serviceName, exists := labels["kubernetes.io/service-name"]; exists {
			parents = append(parents, ParentRef{
				ResourceKey: kube.ResourceKey{
					Group:     "",
					Kind:      "Service",
					Namespace: obj.GetNamespace(),
					Name:      serviceName,
				},
			})
		}

	case schema.GroupKind{Group: "", Kind: "PersistentVolumeClaim"}:
		// PVC → StatefulSet (if created by volumeClaimTemplate)
		// Check for label: statefulset.kubernetes.io/pod-name
		labels := obj.GetLabels()
		if podName, exists := labels["statefulset.kubernetes.io/pod-name"]; exists {
			// Extract StatefulSet name from pod name (format: <sts-name>-<ordinal>)
			stsName := extractStatefulSetName(podName)
			if stsName != "" {
				parents = append(parents, ParentRef{
					ResourceKey: kube.ResourceKey{
						Group:     "apps",
						Kind:      "StatefulSet",
						Namespace: obj.GetNamespace(),
						Name:      stsName,
					},
				})
			}
		}

	case schema.GroupKind{Group: "", Kind: "Secret"}:
		// Token secret → ServiceAccount
		annotations := obj.GetAnnotations()
		if saName, exists := annotations["kubernetes.io/service-account.name"]; exists {
			parents = append(parents, ParentRef{
				ResourceKey: kube.ResourceKey{
					Group:     "",
					Kind:      "ServiceAccount",
					Namespace: obj.GetNamespace(),
					Name:      saName,
				},
			})
		}
	}

	return parents
}

// GetExpectedDescendants returns the expected descendant types for a given resource type.
// This includes both explicit (via OwnerReferences) and implicit descendants.
func (dt *DescendantTracker) GetExpectedDescendants(gk schema.GroupKind) []schema.GroupKind {
	var descendants []schema.GroupKind

	// Add explicit descendants (known OwnerReference patterns)
	explicitDescendants := dt.getExplicitDescendants(gk)
	descendants = append(descendants, explicitDescendants...)

	// Add implicit descendants
	if implicitDescs, exists := dt.implicitRelationships[gk]; exists {
		descendants = append(descendants, implicitDescs...)
	}

	return descendants
}

// getExplicitDescendants returns descendants that use OwnerReferences.
func (dt *DescendantTracker) getExplicitDescendants(gk schema.GroupKind) []schema.GroupKind {
	var descendants []schema.GroupKind

	switch gk {
	case schema.GroupKind{Group: "apps", Kind: "Deployment"}:
		descendants = []schema.GroupKind{
			{Group: "apps", Kind: "ReplicaSet"},
		}

	case schema.GroupKind{Group: "apps", Kind: "ReplicaSet"}:
		descendants = []schema.GroupKind{
			{Group: "", Kind: "Pod"},
		}

	case schema.GroupKind{Group: "apps", Kind: "StatefulSet"}:
		descendants = []schema.GroupKind{
			{Group: "", Kind: "Pod"},
		}

	case schema.GroupKind{Group: "apps", Kind: "DaemonSet"}:
		descendants = []schema.GroupKind{
			{Group: "", Kind: "Pod"},
		}

	case schema.GroupKind{Group: "batch", Kind: "Job"}:
		descendants = []schema.GroupKind{
			{Group: "", Kind: "Pod"},
		}

	case schema.GroupKind{Group: "batch", Kind: "CronJob"}:
		descendants = []schema.GroupKind{
			{Group: "batch", Kind: "Job"},
		}

	case schema.GroupKind{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}:
		// CRDs create custom resources, but we can't know the type ahead of time
		// This will be handled specially by watching for CRD creation events
		// and dynamically adding watches for instances
	}

	return descendants
}

// ExtractReferencedResources extracts resources referenced by a resource (not via OwnerRef).
// For example, Secrets and ConfigMaps referenced by a Pod.
func (dt *DescendantTracker) ExtractReferencedResources(obj *unstructured.Unstructured) []ResourceReference {
	var refs []ResourceReference

	gvk := obj.GroupVersionKind()

	switch gvk.Kind {
	case "Pod":
		refs = append(refs, dt.extractPodReferences(obj)...)

	case "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "ReplicaSet":
		// These have PodTemplateSpec in their spec
		refs = append(refs, dt.extractPodTemplateReferences(obj)...)
	}

	return refs
}

// extractPodReferences extracts Secret and ConfigMap references from a Pod.
func (dt *DescendantTracker) extractPodReferences(obj *unstructured.Unstructured) []ResourceReference {
	var refs []ResourceReference

	namespace := obj.GetNamespace()

	// Extract from volumes
	volumes, found, err := unstructured.NestedSlice(obj.Object, "spec", "volumes")
	if err == nil && found {
		for _, vol := range volumes {
			volMap, ok := vol.(map[string]interface{})
			if !ok {
				continue
			}

			// Check for Secret volume
			if secretMap, found := volMap["secret"].(map[string]interface{}); found {
				if secretName, found := secretMap["secretName"].(string); found {
					refs = append(refs, ResourceReference{
						GroupKind: schema.GroupKind{Group: "", Kind: "Secret"},
						Namespace: namespace,
						Name:      secretName,
						RefType:   RefTypeVolume,
					})
				}
			}

			// Check for ConfigMap volume
			if cmMap, found := volMap["configMap"].(map[string]interface{}); found {
				if cmName, found := cmMap["name"].(string); found {
					refs = append(refs, ResourceReference{
						GroupKind: schema.GroupKind{Group: "", Kind: "ConfigMap"},
						Namespace: namespace,
						Name:      cmName,
						RefType:   RefTypeVolume,
					})
				}
			}
		}
	}

	// Extract from envFrom (containers and initContainers)
	refs = append(refs, dt.extractEnvReferences(obj, namespace, "spec", "containers")...)
	refs = append(refs, dt.extractEnvReferences(obj, namespace, "spec", "initContainers")...)

	return refs
}

// extractPodTemplateReferences extracts references from a PodTemplateSpec.
func (dt *DescendantTracker) extractPodTemplateReferences(obj *unstructured.Unstructured) []ResourceReference {
	var refs []ResourceReference

	namespace := obj.GetNamespace()

	// Most workloads have spec.template.spec.volumes
	basePaths := [][]string{
		{"spec", "template", "spec"},
		{"spec", "jobTemplate", "spec", "template", "spec"}, // CronJob
	}

	for _, basePath := range basePaths {
		// Check volumes
		volumePath := append(basePath, "volumes")
		volumes, found, err := unstructured.NestedSlice(obj.Object, volumePath...)
		if err == nil && found {
			for _, vol := range volumes {
				volMap, ok := vol.(map[string]interface{})
				if !ok {
					continue
				}

				// Secret volume
				if secretMap, found := volMap["secret"].(map[string]interface{}); found {
					if secretName, found := secretMap["secretName"].(string); found {
						refs = append(refs, ResourceReference{
							GroupKind: schema.GroupKind{Group: "", Kind: "Secret"},
							Namespace: namespace,
							Name:      secretName,
							RefType:   RefTypeVolume,
						})
					}
				}

				// ConfigMap volume
				if cmMap, found := volMap["configMap"].(map[string]interface{}); found {
					if cmName, found := cmMap["name"].(string); found {
						refs = append(refs, ResourceReference{
							GroupKind: schema.GroupKind{Group: "", Kind: "ConfigMap"},
							Namespace: namespace,
							Name:      cmName,
							RefType:   RefTypeVolume,
						})
					}
				}
			}
		}

		// Check envFrom in containers
		containerPath := append(basePath, "containers")
		refs = append(refs, dt.extractEnvReferences(obj, namespace, containerPath...)...)

		// Check envFrom in initContainers
		initContainerPath := append(basePath, "initContainers")
		refs = append(refs, dt.extractEnvReferences(obj, namespace, initContainerPath...)...)
	}

	return refs
}

// extractEnvReferences extracts Secret and ConfigMap references from container env/envFrom.
func (dt *DescendantTracker) extractEnvReferences(obj *unstructured.Unstructured, namespace string, containerPath ...string) []ResourceReference {
	var refs []ResourceReference

	containers, found, err := unstructured.NestedSlice(obj.Object, containerPath...)
	if err != nil || !found {
		return refs
	}

	for _, container := range containers {
		containerMap, ok := container.(map[string]interface{})
		if !ok {
			continue
		}

		// Check envFrom
		if envFrom, found := containerMap["envFrom"].([]interface{}); found {
			for _, env := range envFrom {
				envMap, ok := env.(map[string]interface{})
				if !ok {
					continue
				}

				// SecretRef
				if secretRef, found := envMap["secretRef"].(map[string]interface{}); found {
					if secretName, found := secretRef["name"].(string); found {
						refs = append(refs, ResourceReference{
							GroupKind: schema.GroupKind{Group: "", Kind: "Secret"},
							Namespace: namespace,
							Name:      secretName,
							RefType:   RefTypeEnv,
						})
					}
				}

				// ConfigMapRef
				if cmRef, found := envMap["configMapRef"].(map[string]interface{}); found {
					if cmName, found := cmRef["name"].(string); found {
						refs = append(refs, ResourceReference{
							GroupKind: schema.GroupKind{Group: "", Kind: "ConfigMap"},
							Namespace: namespace,
							Name:      cmName,
							RefType:   RefTypeEnv,
						})
					}
				}
			}
		}

		// Check env (individual environment variables)
		if env, found := containerMap["env"].([]interface{}); found {
			for _, e := range env {
				eMap, ok := e.(map[string]interface{})
				if !ok {
					continue
				}

				if valueFrom, found := eMap["valueFrom"].(map[string]interface{}); found {
					// SecretKeyRef
					if secretRef, found := valueFrom["secretKeyRef"].(map[string]interface{}); found {
						if secretName, found := secretRef["name"].(string); found {
							refs = append(refs, ResourceReference{
								GroupKind: schema.GroupKind{Group: "", Kind: "Secret"},
								Namespace: namespace,
								Name:      secretName,
								RefType:   RefTypeEnv,
							})
						}
					}

					// ConfigMapKeyRef
					if cmRef, found := valueFrom["configMapKeyRef"].(map[string]interface{}); found {
						if cmName, found := cmRef["name"].(string); found {
							refs = append(refs, ResourceReference{
								GroupKind: schema.GroupKind{Group: "", Kind: "ConfigMap"},
								Namespace: namespace,
								Name:      cmName,
								RefType:   RefTypeEnv,
							})
						}
					}
				}
			}
		}
	}

	return refs
}

// ResourceReference represents a reference to another resource.
type ResourceReference struct {
	GroupKind schema.GroupKind
	Namespace string
	Name      string
	RefType   ReferenceType
}

// ReferenceType indicates how the resource is referenced.
type ReferenceType string

const (
	RefTypeVolume ReferenceType = "volume"
	RefTypeEnv    ReferenceType = "env"
)

// extractGroup extracts the group from an API version string.
func extractGroup(apiVersion string) string {
	parts := strings.Split(apiVersion, "/")
	if len(parts) == 2 {
		return parts[0]
	}
	return ""
}

// extractStatefulSetName extracts the StatefulSet name from a pod name.
// Pod name format: <statefulset-name>-<ordinal>
func extractStatefulSetName(podName string) string {
	// Find the last dash
	lastDash := strings.LastIndex(podName, "-")
	if lastDash == -1 {
		return ""
	}

	// Check if what follows is a number (ordinal)
	ordinal := podName[lastDash+1:]
	for _, c := range ordinal {
		if c < '0' || c > '9' {
			return "" // Not a number
		}
	}

	return podName[:lastDash]
}

// ToResourceKey converts a ResourceReference to a ResourceKey.
func (r ResourceReference) ToResourceKey() kube.ResourceKey {
	return kube.ResourceKey{
		Group:     r.GroupKind.Group,
		Kind:      r.GroupKind.Kind,
		Namespace: r.Namespace,
		Name:      r.Name,
	}
}

// BuildOwnerReference creates an OwnerReference from a ResourceNode.
func BuildOwnerReference(node *ResourceNode) metav1.OwnerReference {
	version := node.Version
	if version == "" {
		version = "v1"
	}
	apiVersion := schema.GroupVersion{Group: node.Key.Group, Version: version}.String()

	return metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       node.Key.Kind,
		Name:       node.Key.Name,
		UID:        types.UID(node.UID),
	}
}

// String returns a string representation of the ResourceReference.
func (r ResourceReference) String() string {
	gk := r.GroupKind
	group := gk.Group
	if group == "" {
		group = "core"
	}
	return fmt.Sprintf("%s/%s:%s/%s", group, gk.Kind, r.Namespace, r.Name)
}
