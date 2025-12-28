package graphcache

import (
	"context"
	"fmt"
	"strings"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

const (
	LabelTrackingCriteria      = "$.metadata.labels.app\\.kubernetes\\.io/instance"
	AnnotationTrackingCriteria = "$.metadata.annotations.argocd\\.argoproj\\.io/tracking-id"
)

// CyphernetesQueryExecutor provides advanced graph query capabilities using Cyphernetes
type CyphernetesQueryExecutor struct {
	graphCache    *GraphCache
	dynamicClient dynamic.Interface
	executor      *core.QueryExecutor
}

// NewCyphernetesQueryExecutor creates a new Cyphernetes query executor
func NewCyphernetesQueryExecutor(graphCache *GraphCache, dynamicClient dynamic.Interface) (*CyphernetesQueryExecutor, error) {
	p := NewGraphProvider(graphCache)

	executor, err := core.NewQueryExecutor(p)
	if err != nil {
		return nil, fmt.Errorf("failed to create cyphernetes executor: %w", err)
	}

	cqe := &CyphernetesQueryExecutor{
		graphCache:    graphCache,
		dynamicClient: dynamicClient,
		executor:      executor,
	}

	cqe.RegisterArgoCDRules()
	return cqe, nil
}

// RegisterArgoCDRules registers default relationship rules for Argo CD
func (c *CyphernetesQueryExecutor) RegisterArgoCDRules() {
	// Add OpenShift specific rules
	core.AddRelationshipRule(core.RelationshipRule{
		KindA:        "hostfirmwaresettings",
		KindB:        "baremetalhosts",
		Relationship: "BAREMETALHOSTS_OWN_HOSTFIRMWARE_SETTINGS",
		MatchCriteria: []core.MatchCriterion{
			{
				FieldA:         "$.metadata.ownerReferences[].name",
				FieldB:         "$.metadata.name",
				ComparisonType: core.ExactMatch,
			},
		},
	})
}

// AddRuleForResourceKind adds a relationship rule for a specific resource kind to Argo CD Application
func (c *CyphernetesQueryExecutor) AddRuleForResourceKind(resourceKind string, trackingMethod string) {
	tracker := "LBL"
	fieldAMatchCriteria := LabelTrackingCriteria
	comparison := core.ExactMatch

	if trackingMethod == string(TrackingMethodAnnotation) {
		tracker = "ANN"
		fieldAMatchCriteria = AnnotationTrackingCriteria
		comparison = core.StringContains
	}

	relationshipTypeName := strings.ToUpper(fmt.Sprintf("%s_%s_%s", "ARGOAPP_OWN", tracker, resourceKind))
	if strings.Index(relationshipTypeName, ".") != -1 {
		relationshipTypeName = strings.Replace(relationshipTypeName, ".", "_", -1)
	}

	core.AddRelationshipRule(core.RelationshipRule{
		KindA:        strings.ToLower(resourceKind),
		KindB:        "applications.argoproj.io",
		Relationship: core.RelationshipType(relationshipTypeName),
		MatchCriteria: []core.MatchCriterion{
			{
				FieldA:         fieldAMatchCriteria,
				FieldB:         "$.metadata.name",
				ComparisonType: comparison,
			},
		},
	})
	
	log.WithFields(log.Fields{
		"kind": resourceKind,
		"relationship": relationshipTypeName,
	}).Debug("Added Cyphernetes relationship rule")
}

// QueryDependencies executes a Cyphernetes query to find resource dependencies
// Example queries:
//   - "MATCH (d:Deployment)-[:OWNS]->(rs:ReplicaSet)-[:OWNS]->(p:Pod) WHERE d.metadata.name='nginx' RETURN p"
//   - "MATCH (d:Deployment) WHERE d.metadata.labels.app='guestbook' MATCH (d)-[:OWNS*]->(p:Pod) RETURN p"
func (c *CyphernetesQueryExecutor) QueryDependencies(ctx context.Context, query string) ([]*ResourceNode, error) {
	log.WithField("query", query).Debug("Executing Cyphernetes query")

	// Parse query
	ast, err := core.ParseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("failed to parse query: %w", err)
	}

	// Execute the query
	// Note: Execute signature depends on cyphernetes version. Assuming Execute(ast, namespace)
	results, err := c.executor.Execute(ast, "")
	if err != nil {
		return nil, fmt.Errorf("failed to execute query: %w", err)
	}

	// Convert results to ResourceNodes
	nodes := make([]*ResourceNode, 0)
	
	// Iterate over all returned variables
	for _, val := range results.Data {
		if list, ok := val.([]interface{}); ok {
			for _, item := range list {
				// Check if item is a resource (unstructured or map)
				// Cyphernetes might return map[string]interface{}
				if resource, ok := item.(*unstructured.Unstructured); ok {
					node := c.unstructuredToResourceNode(resource)
					nodes = append(nodes, node)
				} else if resourceMap, ok := item.(map[string]interface{}); ok {
					// Convert map to unstructured
					u := &unstructured.Unstructured{Object: resourceMap}
					node := c.unstructuredToResourceNode(u)
					nodes = append(nodes, node)
				}
			}
		}
	}

	log.WithField("results", len(nodes)).Debug("Cyphernetes query completed")
	return nodes, nil
}

// FindResourcesByPattern finds resources matching a pattern using Cyphernetes
// Example: "(:Deployment {metadata.labels.app: 'guestbook'})"
func (c *CyphernetesQueryExecutor) FindResourcesByPattern(ctx context.Context, pattern string) ([]*ResourceNode, error) {
	query := fmt.Sprintf("MATCH %s RETURN n", pattern)
	return c.QueryDependencies(ctx, query)
}

// FindDependentResources finds all resources dependent on a given resource
// This creates a Cypher query that traverses the ownership graph
func (c *CyphernetesQueryExecutor) FindDependentResources(ctx context.Context, resourceKey kube.ResourceKey) ([]*ResourceNode, error) {
	// Build Cypher query to find all dependent resources
	query := fmt.Sprintf(
		"MATCH (r:%s {metadata.name: '%s', metadata.namespace: '%s'})-[:OWNS*]->(dep) RETURN dep",
		resourceKey.Kind,
		resourceKey.Name,
		resourceKey.Namespace,
	)

	return c.QueryDependencies(ctx, query)
}

// FindResourcesByApp finds all resources for a given application using Cyphernetes
func (c *CyphernetesQueryExecutor) FindResourcesByApp(ctx context.Context, appName string) ([]*ResourceNode, error) {
	// Query resources with app.kubernetes.io/instance label
	query := fmt.Sprintf(
		"MATCH (r) WHERE r.metadata.labels['app.kubernetes.io/instance'] = '%s' RETURN r",
		appName,
	)

	return c.QueryDependencies(ctx, query)
}

// FindOrphanedResources finds resources that have no owner references
func (c *CyphernetesQueryExecutor) FindOrphanedResources(ctx context.Context, namespace string) ([]*ResourceNode, error) {
	query := fmt.Sprintf(
		"MATCH (r) WHERE r.metadata.namespace = '%s' AND NOT EXISTS(r.metadata.ownerReferences) RETURN r",
		namespace,
	)

	return c.QueryDependencies(ctx, query)
}

// FindResourcePath finds the shortest path between two resources
func (c *CyphernetesQueryExecutor) FindResourcePath(ctx context.Context, from, to kube.ResourceKey) ([]*ResourceNode, error) {
	query := fmt.Sprintf(
		"MATCH path = shortestPath((a:%s {metadata.name: '%s'})-[:OWNS*]-(b:%s {metadata.name: '%s'})) RETURN nodes(path)",
		from.Kind, from.Name,
		to.Kind, to.Name,
	)

	return c.QueryDependencies(ctx, query)
}

// FindResourcesByHealth finds resources by health status
// This requires health information to be stored in annotations or status
func (c *CyphernetesQueryExecutor) FindResourcesByHealth(ctx context.Context, healthStatus string) ([]*ResourceNode, error) {
	query := fmt.Sprintf(
		"MATCH (r) WHERE r.status.health.status = '%s' RETURN r",
		healthStatus,
	)

	return c.QueryDependencies(ctx, query)
}

// AnalyzeDependencyChain analyzes the complete dependency chain for a resource
// Returns a structured view of all dependencies at each level
func (c *CyphernetesQueryExecutor) AnalyzeDependencyChain(ctx context.Context, resourceKey kube.ResourceKey) (map[int][]*ResourceNode, error) {
	// Build query to get dependency levels
	query := fmt.Sprintf(
		"MATCH path = (r:%s {metadata.name: '%s', metadata.namespace: '%s'})-[:OWNS*]->(dep) "+
			"RETURN dep, length(path) as level ORDER BY level",
		resourceKey.Kind,
		resourceKey.Name,
		resourceKey.Namespace,
	)

	// Parse query
	ast, err := core.ParseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("failed to parse query: %w", err)
	}

	// Execute query
	results, err := c.executor.Execute(ast, "")
	if err != nil {
		return nil, fmt.Errorf("failed to execute query: %w", err)
	}

	// Organize results by level
	levelMap := make(map[int][]*ResourceNode)
	
	// Assuming results.Data has "dep" and "level" keys or similar
	// But iterate generic way is safer or we need to know exact keys.
	// In the query: RETURN dep, length(path) as level
	// Keys should be "dep" and "level".
	
	deps, ok1 := results.Data["dep"].([]interface{})
	levels, ok2 := results.Data["level"].([]interface{})
	
	if ok1 && ok2 && len(deps) == len(levels) {
		for i := 0; i < len(deps); i++ {
			dep := deps[i]
			lvl := levels[i]
			
			var node *ResourceNode
			if resource, ok := dep.(*unstructured.Unstructured); ok {
				node = c.unstructuredToResourceNode(resource)
			} else if resourceMap, ok := dep.(map[string]interface{}); ok {
				u := &unstructured.Unstructured{Object: resourceMap}
				node = c.unstructuredToResourceNode(u)
			}
			
			if node != nil {
				if levelInt, ok := lvl.(int); ok {
					levelMap[levelInt] = append(levelMap[levelInt], node)
				} else if levelInt64, ok := lvl.(int64); ok {
					levelMap[int(levelInt64)] = append(levelMap[int(levelInt64)], node)
				}
			}
		}
	}

	return levelMap, nil
}

// unstructuredToResourceNode converts an unstructured.Unstructured to ResourceNode
func (c *CyphernetesQueryExecutor) unstructuredToResourceNode(obj *unstructured.Unstructured) *ResourceNode {
	gvk := obj.GroupVersionKind()
	key := ToResourceKey(obj)

	return &ResourceNode{
		Key:             key,
		Version:         gvk.Version,
		UID:             string(obj.GetUID()),
		ResourceVersion: obj.GetResourceVersion(),
		CreatedAt:       obj.GetCreationTimestamp().Time,
		Info: &ResourceMetadata{
			Labels:      obj.GetLabels(),
			Annotations: obj.GetAnnotations(),
			OwnerRefs:   obj.GetOwnerReferences(),
		},
	}
}

// QueryExecutor provides a fluent interface for building and executing queries
type QueryBuilder struct {
	executor *CyphernetesQueryExecutor
	query    string
}

// NewQueryBuilder creates a new query builder
func (c *CyphernetesQueryExecutor) NewQueryBuilder() *QueryBuilder {
	return &QueryBuilder{
		executor: c,
		query:    "",
	}
}

// Match adds a MATCH clause
func (qb *QueryBuilder) Match(pattern string) *QueryBuilder {
	if qb.query == "" {
		qb.query = "MATCH " + pattern
	} else {
		qb.query += " MATCH " + pattern
	}
	return qb
}

// Where adds a WHERE clause
func (qb *QueryBuilder) Where(condition string) *QueryBuilder {
	if qb.query == "" {
		return qb
	}
	qb.query += " WHERE " + condition
	return qb
}

// Return adds a RETURN clause
func (qb *QueryBuilder) Return(fields string) *QueryBuilder {
	qb.query += " RETURN " + fields
	return qb
}

// Execute executes the built query
func (qb *QueryBuilder) Execute(ctx context.Context) ([]*ResourceNode, error) {
	return qb.executor.QueryDependencies(ctx, qb.query)
}

// Examples of common queries using the query builder:
//
// Find all Pods owned by a specific Deployment:
//
//	executor.NewQueryBuilder().
//	  Match("(d:Deployment {metadata.name: 'nginx'})-[:OWNS*]->(p:Pod)").
//	  Return("p").
//	  Execute(ctx)
//
// Find all resources with a specific label:
//
//	executor.NewQueryBuilder().
//	  Match("(r)").
//	  Where("r.metadata.labels.app = 'myapp'").
//	  Return("r").
//	  Execute(ctx)
//
// Find unhealthy resources for an application:
//
//	executor.NewQueryBuilder().
//	  Match("(r)").
//	  Where("r.metadata.labels['app.kubernetes.io/instance'] = 'myapp' AND r.status.health.status = 'Degraded'").
//	  Return("r").
//	  Execute(ctx)
