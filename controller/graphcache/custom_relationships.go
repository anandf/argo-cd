package graphcache

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// ComparisonType defines how field values are compared in custom relationship rules.
type ComparisonType string

const (
	ComparisonEquals         ComparisonType = "Equals"
	ComparisonStringContains ComparisonType = "StringContains"
	ComparisonRegex          ComparisonType = "Regex"
)

// ResourceSelector identifies a class of Kubernetes resources by group and kind.
type ResourceSelector struct {
	Group string `json:"group" yaml:"group"`
	Kind  string `json:"kind" yaml:"kind"`
}

// FieldMatchRule defines a single field comparison between parent and child resources.
type FieldMatchRule struct {
	ParentField string         `json:"parentField" yaml:"parentField"`
	ChildField  string         `json:"childField" yaml:"childField"`
	Comparison  ComparisonType `json:"comparison" yaml:"comparison"`
}

// CustomRelationshipRule defines a custom parent-child relationship between resources.
type CustomRelationshipRule struct {
	Name           string           `json:"name" yaml:"name"`
	ParentResource ResourceSelector `json:"parentResource" yaml:"parentResource"`
	ChildResource  ResourceSelector `json:"childResource" yaml:"childResource"`
	MatchRules     []FieldMatchRule `json:"matchRules" yaml:"matchRules"`
}

// CustomRelationshipIndex indexes rules by GroupKind for fast lookup.
type CustomRelationshipIndex struct {
	mu                  sync.RWMutex
	byParentGK          map[schema.GroupKind][]*CustomRelationshipRule
	byChildGK           map[schema.GroupKind][]*CustomRelationshipRule
	wildcardParentRules []*CustomRelationshipRule
	wildcardChildRules  []*CustomRelationshipRule
	allRules            []*CustomRelationshipRule
	compiledRegex       map[string]*regexp.Regexp
}

// NewCustomRelationshipIndex creates an index from a list of rules.
func NewCustomRelationshipIndex(rules []CustomRelationshipRule) *CustomRelationshipIndex {
	idx := &CustomRelationshipIndex{
		byParentGK:    make(map[schema.GroupKind][]*CustomRelationshipRule),
		byChildGK:     make(map[schema.GroupKind][]*CustomRelationshipRule),
		allRules:      make([]*CustomRelationshipRule, len(rules)),
		compiledRegex: make(map[string]*regexp.Regexp),
	}

	for i := range rules {
		rule := &rules[i]
		idx.allRules[i] = rule

		if isWildcard(rule.ParentResource) {
			idx.wildcardParentRules = append(idx.wildcardParentRules, rule)
		} else {
			gk := schema.GroupKind{Group: rule.ParentResource.Group, Kind: rule.ParentResource.Kind}
			idx.byParentGK[gk] = append(idx.byParentGK[gk], rule)
		}

		if isWildcard(rule.ChildResource) {
			idx.wildcardChildRules = append(idx.wildcardChildRules, rule)
		} else {
			gk := schema.GroupKind{Group: rule.ChildResource.Group, Kind: rule.ChildResource.Kind}
			idx.byChildGK[gk] = append(idx.byChildGK[gk], rule)
		}

		for _, mr := range rule.MatchRules {
			if mr.Comparison == ComparisonRegex {
				if compiled, err := regexp.Compile(mr.ParentField); err == nil {
					idx.compiledRegex[mr.ParentField] = compiled
				}
				if compiled, err := regexp.Compile(mr.ChildField); err == nil {
					idx.compiledRegex[mr.ChildField] = compiled
				}
			}
		}
	}

	return idx
}

// GetRulesAsParent returns all rules where gk matches the parent selector.
func (idx *CustomRelationshipIndex) GetRulesAsParent(gk schema.GroupKind) []*CustomRelationshipRule {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var result []*CustomRelationshipRule
	result = append(result, idx.byParentGK[gk]...)
	for _, rule := range idx.wildcardParentRules {
		if selectorMatchesGK(rule.ParentResource, gk) {
			result = append(result, rule)
		}
	}
	return result
}

// GetRulesAsChild returns all rules where gk matches the child selector.
func (idx *CustomRelationshipIndex) GetRulesAsChild(gk schema.GroupKind) []*CustomRelationshipRule {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var result []*CustomRelationshipRule
	result = append(result, idx.byChildGK[gk]...)
	for _, rule := range idx.wildcardChildRules {
		if selectorMatchesGK(rule.ChildResource, gk) {
			result = append(result, rule)
		}
	}
	return result
}

// GetAllReferencedGroupKinds returns all concrete GroupKinds mentioned in rules.
func (idx *CustomRelationshipIndex) GetAllReferencedGroupKinds() []schema.GroupKind {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	seen := make(map[schema.GroupKind]bool)
	for _, rule := range idx.allRules {
		if !isWildcard(rule.ParentResource) {
			gk := schema.GroupKind{Group: rule.ParentResource.Group, Kind: rule.ParentResource.Kind}
			seen[gk] = true
		}
		if !isWildcard(rule.ChildResource) {
			gk := schema.GroupKind{Group: rule.ChildResource.Group, Kind: rule.ChildResource.Kind}
			seen[gk] = true
		}
	}

	result := make([]schema.GroupKind, 0, len(seen))
	for gk := range seen {
		result = append(result, gk)
	}
	return result
}

func isWildcard(sel ResourceSelector) bool {
	return sel.Group == "*" || sel.Kind == "*"
}

func selectorMatchesGK(sel ResourceSelector, gk schema.GroupKind) bool {
	if sel.Group != "*" && sel.Group != gk.Group {
		return false
	}
	if sel.Kind != "*" && sel.Kind != gk.Kind {
		return false
	}
	return true
}

func selectorToGroupKind(sel ResourceSelector) *schema.GroupKind {
	if isWildcard(sel) {
		return nil
	}
	return &schema.GroupKind{Group: sel.Group, Kind: sel.Kind}
}

// parseFieldPath parses a JSONPath-like field path into segments.
// Supports dot-notation and bracket-notation for keys with special characters.
//
// Examples:
//
//	"$.metadata.name"                                         → ["metadata", "name"]
//	"$.metadata.annotations['argocd.argoproj.io/tracking-id']" → ["metadata", "annotations", "argocd.argoproj.io/tracking-id"]
//	"metadata.labels['app']"                                  → ["metadata", "labels", "app"]
func parseFieldPath(path string) []string {
	path = strings.TrimPrefix(path, "$.")
	path = strings.TrimPrefix(path, "$")

	if path == "" {
		return nil
	}

	var segments []string
	var current strings.Builder

	i := 0
	for i < len(path) {
		ch := path[i]
		switch {
		case ch == '.':
			if current.Len() > 0 {
				segments = append(segments, current.String())
				current.Reset()
			}
			i++
		case ch == '[':
			if current.Len() > 0 {
				segments = append(segments, current.String())
				current.Reset()
			}
			i++
			// Read bracket content
			quote := byte(0)
			if i < len(path) && (path[i] == '\'' || path[i] == '"') {
				quote = path[i]
				i++
			}
			for i < len(path) {
				if quote != 0 && path[i] == quote {
					i++ // skip closing quote
					break
				}
				if quote == 0 && path[i] == ']' {
					break
				}
				current.WriteByte(path[i])
				i++
			}
			if i < len(path) && path[i] == ']' {
				i++
			}
			segments = append(segments, current.String())
			current.Reset()
		default:
			current.WriteByte(ch)
			i++
		}
	}

	if current.Len() > 0 {
		segments = append(segments, current.String())
	}

	return segments
}

// resolveFieldPath extracts a string value from an unstructured object using a JSONPath-like path.
func resolveFieldPath(obj *unstructured.Unstructured, path string) (string, bool) {
	if obj == nil || path == "" {
		return "", false
	}

	segments := parseFieldPath(path)
	if len(segments) == 0 {
		return "", false
	}

	val, found, err := unstructured.NestedFieldNoCopy(obj.Object, segments...)
	if err != nil || !found || val == nil {
		return "", false
	}

	return fmt.Sprintf("%v", val), true
}

// compareFields compares two field values using the specified comparison type.
func compareFields(parentVal, childVal string, comparison ComparisonType, compiledRegex map[string]*regexp.Regexp) bool {
	switch comparison {
	case ComparisonEquals:
		return parentVal == childVal
	case ComparisonStringContains:
		return strings.Contains(parentVal, childVal)
	case ComparisonRegex:
		if re, ok := compiledRegex[childVal]; ok {
			return re.MatchString(parentVal)
		}
		re, err := regexp.Compile(childVal)
		if err != nil {
			return false
		}
		return re.MatchString(parentVal)
	default:
		return false
	}
}

// evaluateCustomRelationship tests whether a parent and child resource satisfy
// all match rules in a custom relationship rule. Uses AND semantics.
func evaluateCustomRelationship(rule *CustomRelationshipRule, parent, child *unstructured.Unstructured, compiledRegex map[string]*regexp.Regexp) bool {
	if parent == nil || child == nil {
		return false
	}

	for _, mr := range rule.MatchRules {
		parentVal, parentFound := resolveFieldPath(parent, mr.ParentField)
		childVal, childFound := resolveFieldPath(child, mr.ChildField)

		if !parentFound || !childFound {
			return false
		}

		if !compareFields(parentVal, childVal, mr.Comparison, compiledRegex) {
			return false
		}
	}

	return len(rule.MatchRules) > 0
}

// ParseCustomRelationshipRules parses and validates custom relationship rules from YAML.
func ParseCustomRelationshipRules(yamlData string) ([]CustomRelationshipRule, error) {
	if yamlData == "" {
		return nil, nil
	}

	var rules []CustomRelationshipRule
	if err := yaml.Unmarshal([]byte(yamlData), &rules); err != nil {
		return nil, fmt.Errorf("error unmarshalling resource relationships: %w", err)
	}

	for i, rule := range rules {
		if rule.Name == "" {
			return nil, fmt.Errorf("rule %d: name is required", i)
		}
		if len(rule.MatchRules) == 0 {
			return nil, fmt.Errorf("rule %q: at least one matchRule is required", rule.Name)
		}
		for j, mr := range rule.MatchRules {
			if mr.ParentField == "" || mr.ChildField == "" {
				return nil, fmt.Errorf("rule %q, matchRule %d: parentField and childField are required", rule.Name, j)
			}
			switch mr.Comparison {
			case ComparisonEquals, ComparisonStringContains, ComparisonRegex:
			default:
				return nil, fmt.Errorf("rule %q, matchRule %d: invalid comparison type %q", rule.Name, j, mr.Comparison)
			}
		}
	}

	return rules, nil
}
