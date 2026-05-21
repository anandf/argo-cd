package graphcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseFieldPath_SimpleDotNotation(t *testing.T) {
	assert.Equal(t, []string{"metadata", "name"}, parseFieldPath("$.metadata.name"))
}

func TestParseFieldPath_BracketNotation(t *testing.T) {
	result := parseFieldPath("$.metadata.annotations['argocd.argoproj.io/tracking-id']")
	assert.Equal(t, []string{"metadata", "annotations", "argocd.argoproj.io/tracking-id"}, result)
}

func TestParseFieldPath_NoDollarPrefix(t *testing.T) {
	assert.Equal(t, []string{"metadata", "name"}, parseFieldPath("metadata.name"))
}

func TestParseFieldPath_MixedNotation(t *testing.T) {
	result := parseFieldPath("$.spec.selector.matchLabels['app']")
	assert.Equal(t, []string{"spec", "selector", "matchLabels", "app"}, result)
}

func TestParseFieldPath_DoubleQuotes(t *testing.T) {
	result := parseFieldPath(`$.metadata.annotations["key.with.dots"]`)
	assert.Equal(t, []string{"metadata", "annotations", "key.with.dots"}, result)
}

func TestParseFieldPath_Nested(t *testing.T) {
	result := parseFieldPath("$.spec.template.spec.containers")
	assert.Equal(t, []string{"spec", "template", "spec", "containers"}, result)
}

func TestParseFieldPath_Empty(t *testing.T) {
	assert.Nil(t, parseFieldPath(""))
	assert.Nil(t, parseFieldPath("$"))
	assert.Nil(t, parseFieldPath("$."))
}

func TestResolveFieldPath_SimpleField(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      "my-deploy",
			"namespace": "default",
		},
	}}

	val, found := resolveFieldPath(obj, "$.metadata.name")
	assert.True(t, found)
	assert.Equal(t, "my-deploy", val)
}

func TestResolveFieldPath_AnnotationWithDots(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"argocd.argoproj.io/tracking-id": "myapp:apps/Deployment:default/nginx",
			},
		},
	}}

	val, found := resolveFieldPath(obj, "$.metadata.annotations['argocd.argoproj.io/tracking-id']")
	assert.True(t, found)
	assert.Equal(t, "myapp:apps/Deployment:default/nginx", val)
}

func TestResolveFieldPath_MissingField(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "test"},
	}}

	_, found := resolveFieldPath(obj, "$.metadata.labels['app']")
	assert.False(t, found)
}

func TestResolveFieldPath_NestedField(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "nginx",
				},
			},
		},
	}}

	val, found := resolveFieldPath(obj, "$.spec.selector.matchLabels.app")
	assert.True(t, found)
	assert.Equal(t, "nginx", val)
}

func TestResolveFieldPath_NilObject(t *testing.T) {
	_, found := resolveFieldPath(nil, "$.metadata.name")
	assert.False(t, found)
}

func TestCompareFields_Equals_Match(t *testing.T) {
	assert.True(t, compareFields("hello", "hello", ComparisonEquals, nil))
}

func TestCompareFields_Equals_NoMatch(t *testing.T) {
	assert.False(t, compareFields("hello", "world", ComparisonEquals, nil))
}

func TestCompareFields_StringContains_Match(t *testing.T) {
	assert.True(t, compareFields("myapp:apps/Deployment:default/nginx", "myapp", ComparisonStringContains, nil))
}

func TestCompareFields_StringContains_NoMatch(t *testing.T) {
	assert.False(t, compareFields("hello world", "xyz", ComparisonStringContains, nil))
}

func TestCompareFields_Regex_Match(t *testing.T) {
	assert.True(t, compareFields("myapp-v1-abc123", `myapp-v\d+`, ComparisonRegex, nil))
}

func TestCompareFields_Regex_NoMatch(t *testing.T) {
	assert.False(t, compareFields("hello", `^world$`, ComparisonRegex, nil))
}

func TestCompareFields_Regex_InvalidPattern(t *testing.T) {
	assert.False(t, compareFields("hello", `[invalid`, ComparisonRegex, nil))
}

func TestCustomRelationshipIndex_ConcreteRules(t *testing.T) {
	rules := []CustomRelationshipRule{
		{
			Name:           "deploy-to-rs",
			ParentResource: ResourceSelector{Group: "apps", Kind: "Deployment"},
			ChildResource:  ResourceSelector{Group: "apps", Kind: "ReplicaSet"},
			MatchRules: []FieldMatchRule{
				{ParentField: "$.metadata.name", ChildField: "$.metadata.name", Comparison: ComparisonStringContains},
			},
		},
	}

	idx := NewCustomRelationshipIndex(rules)

	parentRules := idx.GetRulesAsParent(schema.GroupKind{Group: "apps", Kind: "Deployment"})
	assert.Len(t, parentRules, 1)

	childRules := idx.GetRulesAsChild(schema.GroupKind{Group: "apps", Kind: "ReplicaSet"})
	assert.Len(t, childRules, 1)

	noRules := idx.GetRulesAsParent(schema.GroupKind{Group: "", Kind: "Pod"})
	assert.Len(t, noRules, 0)
}

func TestCustomRelationshipIndex_WildcardParent(t *testing.T) {
	rules := []CustomRelationshipRule{
		{
			Name:           "any-to-app",
			ParentResource: ResourceSelector{Group: "*", Kind: "*"},
			ChildResource:  ResourceSelector{Group: "argoproj.io", Kind: "Application"},
			MatchRules: []FieldMatchRule{
				{ParentField: "$.metadata.name", ChildField: "$.metadata.name", Comparison: ComparisonEquals},
			},
		},
	}

	idx := NewCustomRelationshipIndex(rules)

	parentRules := idx.GetRulesAsParent(schema.GroupKind{Group: "apps", Kind: "Deployment"})
	assert.Len(t, parentRules, 1)

	parentRules = idx.GetRulesAsParent(schema.GroupKind{Group: "", Kind: "Pod"})
	assert.Len(t, parentRules, 1)

	childRules := idx.GetRulesAsChild(schema.GroupKind{Group: "argoproj.io", Kind: "Application"})
	assert.Len(t, childRules, 1)
}

func TestCustomRelationshipIndex_WildcardKind(t *testing.T) {
	rules := []CustomRelationshipRule{
		{
			Name:           "any-argoproj",
			ParentResource: ResourceSelector{Group: "argoproj.io", Kind: "*"},
			ChildResource:  ResourceSelector{Group: "", Kind: "ConfigMap"},
			MatchRules: []FieldMatchRule{
				{ParentField: "$.metadata.name", ChildField: "$.metadata.name", Comparison: ComparisonEquals},
			},
		},
	}

	idx := NewCustomRelationshipIndex(rules)

	parentRules := idx.GetRulesAsParent(schema.GroupKind{Group: "argoproj.io", Kind: "Application"})
	assert.Len(t, parentRules, 1)

	parentRules = idx.GetRulesAsParent(schema.GroupKind{Group: "argoproj.io", Kind: "Rollout"})
	assert.Len(t, parentRules, 1)

	parentRules = idx.GetRulesAsParent(schema.GroupKind{Group: "apps", Kind: "Deployment"})
	assert.Len(t, parentRules, 0)
}

func TestCustomRelationshipIndex_GetAllReferencedGroupKinds(t *testing.T) {
	rules := []CustomRelationshipRule{
		{
			Name:           "rule1",
			ParentResource: ResourceSelector{Group: "apps", Kind: "Deployment"},
			ChildResource:  ResourceSelector{Group: "apps", Kind: "ReplicaSet"},
			MatchRules:     []FieldMatchRule{{ParentField: "a", ChildField: "b", Comparison: ComparisonEquals}},
		},
		{
			Name:           "rule2",
			ParentResource: ResourceSelector{Group: "*", Kind: "*"},
			ChildResource:  ResourceSelector{Group: "argoproj.io", Kind: "Application"},
			MatchRules:     []FieldMatchRule{{ParentField: "a", ChildField: "b", Comparison: ComparisonEquals}},
		},
	}

	idx := NewCustomRelationshipIndex(rules)
	gks := idx.GetAllReferencedGroupKinds()

	assert.Len(t, gks, 3)
	gkSet := make(map[schema.GroupKind]bool)
	for _, gk := range gks {
		gkSet[gk] = true
	}
	assert.True(t, gkSet[schema.GroupKind{Group: "apps", Kind: "Deployment"}])
	assert.True(t, gkSet[schema.GroupKind{Group: "apps", Kind: "ReplicaSet"}])
	assert.True(t, gkSet[schema.GroupKind{Group: "argoproj.io", Kind: "Application"}])
}

func TestCustomRelationshipIndex_Empty(t *testing.T) {
	var idx *CustomRelationshipIndex
	assert.Nil(t, idx.GetRulesAsParent(schema.GroupKind{}))
	assert.Nil(t, idx.GetRulesAsChild(schema.GroupKind{}))
	assert.Nil(t, idx.GetAllReferencedGroupKinds())
}

func TestEvaluateCustomRelationship_AllMatch(t *testing.T) {
	rule := &CustomRelationshipRule{
		Name: "test",
		MatchRules: []FieldMatchRule{
			{ParentField: "$.metadata.name", ChildField: "$.metadata.name", Comparison: ComparisonEquals},
		},
	}

	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "nginx"},
	}}
	child := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "nginx"},
	}}

	assert.True(t, evaluateCustomRelationship(rule, parent, child, nil))
}

func TestEvaluateCustomRelationship_OneFails(t *testing.T) {
	rule := &CustomRelationshipRule{
		Name: "test",
		MatchRules: []FieldMatchRule{
			{ParentField: "$.metadata.name", ChildField: "$.metadata.name", Comparison: ComparisonEquals},
			{ParentField: "$.metadata.namespace", ChildField: "$.metadata.namespace", Comparison: ComparisonEquals},
		},
	}

	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "nginx", "namespace": "prod"},
	}}
	child := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "nginx", "namespace": "dev"},
	}}

	assert.False(t, evaluateCustomRelationship(rule, parent, child, nil))
}

func TestEvaluateCustomRelationship_MissingField(t *testing.T) {
	rule := &CustomRelationshipRule{
		Name: "test",
		MatchRules: []FieldMatchRule{
			{ParentField: "$.metadata.labels['app']", ChildField: "$.metadata.name", Comparison: ComparisonEquals},
		},
	}

	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "nginx"},
	}}
	child := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "nginx"},
	}}

	assert.False(t, evaluateCustomRelationship(rule, parent, child, nil))
}

func TestEvaluateCustomRelationship_NilResources(t *testing.T) {
	rule := &CustomRelationshipRule{
		Name:       "test",
		MatchRules: []FieldMatchRule{{ParentField: "a", ChildField: "b", Comparison: ComparisonEquals}},
	}
	assert.False(t, evaluateCustomRelationship(rule, nil, nil, nil))
}

func TestEvaluateCustomRelationship_StringContains(t *testing.T) {
	rule := &CustomRelationshipRule{
		Name: "tracking-annotation",
		MatchRules: []FieldMatchRule{
			{
				ParentField: "$.metadata.annotations['argocd.argoproj.io/tracking-id']",
				ChildField:  "$.metadata.name",
				Comparison:  ComparisonStringContains,
			},
		},
	}

	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"argocd.argoproj.io/tracking-id": "guestbook:apps/Deployment:default/nginx",
			},
		},
	}}
	child := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "guestbook"},
	}}

	assert.True(t, evaluateCustomRelationship(rule, parent, child, nil))
}

func TestParseCustomRelationshipRules_Valid(t *testing.T) {
	yaml := `
- name: "test rule"
  parentResource:
    group: "apps"
    kind: "Deployment"
  childResource:
    group: "apps"
    kind: "ReplicaSet"
  matchRules:
    - parentField: "$.metadata.name"
      childField: "$.metadata.name"
      comparison: StringContains
`
	rules, err := ParseCustomRelationshipRules(yaml)
	require.NoError(t, err)
	assert.Len(t, rules, 1)
	assert.Equal(t, "test rule", rules[0].Name)
	assert.Equal(t, "apps", rules[0].ParentResource.Group)
	assert.Equal(t, ComparisonStringContains, rules[0].MatchRules[0].Comparison)
}

func TestParseCustomRelationshipRules_Empty(t *testing.T) {
	rules, err := ParseCustomRelationshipRules("")
	require.NoError(t, err)
	assert.Nil(t, rules)
}

func TestParseCustomRelationshipRules_InvalidYAML(t *testing.T) {
	_, err := ParseCustomRelationshipRules("invalid{yaml]")
	assert.Error(t, err)
}

func TestParseCustomRelationshipRules_MissingName(t *testing.T) {
	yaml := `
- parentResource:
    group: "apps"
    kind: "Deployment"
  childResource:
    group: "apps"
    kind: "ReplicaSet"
  matchRules:
    - parentField: "a"
      childField: "b"
      comparison: Equals
`
	_, err := ParseCustomRelationshipRules(yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
}

func TestParseCustomRelationshipRules_NoMatchRules(t *testing.T) {
	yaml := `
- name: "bad rule"
  parentResource:
    group: "apps"
    kind: "Deployment"
  childResource:
    group: "apps"
    kind: "ReplicaSet"
`
	_, err := ParseCustomRelationshipRules(yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at least one matchRule")
}

func TestParseCustomRelationshipRules_InvalidComparison(t *testing.T) {
	yaml := `
- name: "bad comparison"
  parentResource:
    group: "apps"
    kind: "Deployment"
  childResource:
    group: "apps"
    kind: "ReplicaSet"
  matchRules:
    - parentField: "a"
      childField: "b"
      comparison: InvalidType
`
	_, err := ParseCustomRelationshipRules(yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid comparison type")
}

func TestParseCustomRelationshipRules_MissingFields(t *testing.T) {
	yaml := `
- name: "missing fields"
  parentResource:
    group: "apps"
    kind: "Deployment"
  childResource:
    group: "apps"
    kind: "ReplicaSet"
  matchRules:
    - parentField: ""
      childField: "b"
      comparison: Equals
`
	_, err := ParseCustomRelationshipRules(yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parentField and childField are required")
}

func TestSelectorMatchesGK(t *testing.T) {
	tests := []struct {
		name     string
		sel      ResourceSelector
		gk       schema.GroupKind
		expected bool
	}{
		{"exact match", ResourceSelector{Group: "apps", Kind: "Deployment"}, schema.GroupKind{Group: "apps", Kind: "Deployment"}, true},
		{"group mismatch", ResourceSelector{Group: "batch", Kind: "Deployment"}, schema.GroupKind{Group: "apps", Kind: "Deployment"}, false},
		{"kind mismatch", ResourceSelector{Group: "apps", Kind: "StatefulSet"}, schema.GroupKind{Group: "apps", Kind: "Deployment"}, false},
		{"wildcard group", ResourceSelector{Group: "*", Kind: "Deployment"}, schema.GroupKind{Group: "apps", Kind: "Deployment"}, true},
		{"wildcard kind", ResourceSelector{Group: "apps", Kind: "*"}, schema.GroupKind{Group: "apps", Kind: "Deployment"}, true},
		{"wildcard both", ResourceSelector{Group: "*", Kind: "*"}, schema.GroupKind{Group: "apps", Kind: "Deployment"}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, selectorMatchesGK(tc.sel, tc.gk))
		})
	}
}

func TestSelectorToGroupKind(t *testing.T) {
	gk := selectorToGroupKind(ResourceSelector{Group: "apps", Kind: "Deployment"})
	assert.NotNil(t, gk)
	assert.Equal(t, schema.GroupKind{Group: "apps", Kind: "Deployment"}, *gk)

	assert.Nil(t, selectorToGroupKind(ResourceSelector{Group: "*", Kind: "Deployment"}))
	assert.Nil(t, selectorToGroupKind(ResourceSelector{Group: "apps", Kind: "*"}))
	assert.Nil(t, selectorToGroupKind(ResourceSelector{Group: "*", Kind: "*"}))
}
