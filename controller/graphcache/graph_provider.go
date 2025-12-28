package graphcache

import (
	"fmt"
	"strings"

	"github.com/avitaltamir/cyphernetes/pkg/provider"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
)

// GraphProvider implements cyphernetes.Provider backed by the in-memory GraphCache
type GraphProvider struct {
	cache *GraphCache
}

// NewGraphProvider creates a new in-memory graph provider
func NewGraphProvider(cache *GraphCache) *GraphProvider {
	return &GraphProvider{
		cache: cache,
	}
}

// GetK8sResources returns resources from the cache matching the criteria
func (p *GraphProvider) GetK8sResources(kind, fieldSelector, labelSelector, namespace string) (interface{}, error) {
	// Parse selectors
	lSelector, err := labels.Parse(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid label selector: %w", err)
	}

	// Simple field selector parsing (only supports metadata.name for now)
	var nameSelector string
	if fieldSelector != "" {
		parts := strings.Split(fieldSelector, "=")
		if len(parts) == 2 && (parts[0] == "metadata.name" || parts[0] == "name") {
			nameSelector = parts[1]
		}
	}

	// Resolve GVK from Kind
	allTypes := p.cache.GetAllTypes()
	var targetGKs []schema.GroupKind

	for _, gk := range allTypes {
		if strings.EqualFold(gk.Kind, kind) {
			targetGKs = append(targetGKs, gk)
		}
	}

	if len(targetGKs) == 0 {
		if kind == "" {
			return nil, fmt.Errorf("kind required")
		}
		return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{}}, nil
	}

	// Optimization: Use Label Index if possible
	var candidates []*ResourceNode
	useLabelIndex := false

	requirements, selectable := lSelector.Requirements()
	if selectable {
		for _, req := range requirements {
			if req.Operator() == selection.Equals || req.Operator() == selection.DoubleEquals {
				if len(req.Values()) == 1 {
					val := req.Values().List()[0]
					candidates = p.cache.GetByLabel(req.Key(), val)
					useLabelIndex = true
					break
				}
			}
		}
	}

	var result []unstructured.Unstructured
	
	if useLabelIndex {
		// Filter candidates by Kind
		for _, node := range candidates {
			matchKind := false
			for _, gk := range targetGKs {
				if node.Key.Kind == gk.Kind && node.Key.Group == gk.Group {
					matchKind = true
					break
				}
			}
			if !matchKind {
				continue
			}

			// Apply common filters
			if filterNode(node, namespace, nameSelector, lSelector) {
				u := p.nodeToUnstructured(node)
				result = append(result, *u)
			}
		}
	} else {
		// Fallback: Iterate by Type
		for _, gk := range targetGKs {
			nodes := p.cache.GetByType(gk)
			for _, node := range nodes {
				if filterNode(node, namespace, nameSelector, lSelector) {
					u := p.nodeToUnstructured(node)
					result = append(result, *u)
				}
			}
		}
	}

	return &unstructured.UnstructuredList{Items: result}, nil
}

func filterNode(node *ResourceNode, namespace, nameSelector string, lSelector labels.Selector) bool {
	// Filter by Namespace
	if namespace != "" && node.Key.Namespace != namespace {
		return false
	}

	// Filter by Name
	if nameSelector != "" && node.Key.Name != nameSelector {
		return false
	}

	// Filter by Label Selector
	if !lSelector.Empty() {
		// Ensure Info is not nil
		if node.Info == nil {
			return false
		}
		nodeLabels := labels.Set(node.Info.Labels)
		if !lSelector.Matches(nodeLabels) {
			return false
		}
	}
	return true
}

func (p *GraphProvider) nodeToUnstructured(node *ResourceNode) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   node.Key.Group,
		Version: node.Version,
		Kind:    node.Key.Kind,
	})
	u.SetName(node.Key.Name)
	u.SetNamespace(node.Key.Namespace)
	u.SetUID(types.UID(node.UID))
	
	u.SetResourceVersion(node.ResourceVersion)
	u.SetCreationTimestamp(metav1.NewTime(node.CreatedAt))
	if node.Info != nil {
		u.SetLabels(node.Info.Labels)
		u.SetAnnotations(node.Info.Annotations)
		u.SetOwnerReferences(node.Info.OwnerRefs)
	}
	
	return u
}

// DeleteK8sResources - Read Only
func (p *GraphProvider) DeleteK8sResources(kind, name, namespace string) error {
	return fmt.Errorf("graph provider is read-only")
}

// CreateK8sResource - Read Only
func (p *GraphProvider) CreateK8sResource(kind, name, namespace string, body interface{}) error {
	return fmt.Errorf("graph provider is read-only")
}

// PatchK8sResource - Read Only
func (p *GraphProvider) PatchK8sResource(kind, name, namespace string, patchJSON []byte) error {
	return fmt.Errorf("graph provider is read-only")
}

// FindGVR finds the GroupVersionResource for a kind
func (p *GraphProvider) FindGVR(kind string) (schema.GroupVersionResource, error) {
	// Look up in cache types
	allTypes := p.cache.GetAllTypes()
	for _, gk := range allTypes {
		if strings.EqualFold(gk.Kind, kind) {
			nodes := p.cache.GetByType(gk)
			if len(nodes) > 0 {
				return schema.GroupVersionResource{
					Group:    gk.Group,
					Version:  nodes[0].Version,
					Resource: strings.ToLower(kind) + "s",
				}, nil
			}
			return schema.GroupVersionResource{
				Group:    gk.Group,
				Version:  "",
				Resource: kind,
			}, nil
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("kind %s not found in cache", kind)
}

// GetOpenAPIResourceSpecs returns specs - Not implemented
func (p *GraphProvider) GetOpenAPIResourceSpecs() (map[string][]string, error) {
	return map[string][]string{}, nil
}

// CreateProviderForContext returns self - context switching not supported in single-graph provider
func (p *GraphProvider) CreateProviderForContext(context string) (provider.Provider, error) {
	return p, nil
}

// ToggleDryRun - No-op
func (p *GraphProvider) ToggleDryRun() {}