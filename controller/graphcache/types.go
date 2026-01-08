package graphcache

import (
	"hash/fnv"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
)

const ShardCount = 32 // Deprecated: Use GraphConfig.ShardCount instead

// GraphConfig contains tunable parameters for the graph cache.
// These values allow optimization for different cluster sizes and performance requirements.
type GraphConfig struct {
	// Sharding
	ShardCount int // Number of shards for the resource graph (default: 32)

	// Discovery
	DiscoveryInterval     time.Duration // Interval for periodic resource discovery (default: 5m)
	InitialDiscoveryDelay time.Duration // Delay before first discovery run (default: 10s)

	// Metrics
	MetricsExportInterval time.Duration // Interval for exporting Prometheus metrics (default: 30s)

	// Persistence
	PersistenceInterval time.Duration // Interval for persisting graph state (default: 1m)

	// Watch retry
	MaxConsecutiveFailures int           // Max consecutive watch failures before stopping (default: 100)
	MinRetryInterval       time.Duration // Minimum retry interval for failed watches (default: 1s)
	MaxRetryInterval       time.Duration // Maximum retry interval for failed watches (default: 30s)
}

// DefaultGraphConfig returns the default configuration for the graph cache.
// These defaults are optimized for medium-sized clusters (100-500 applications).
func DefaultGraphConfig() GraphConfig {
	return GraphConfig{
		ShardCount:             32,
		DiscoveryInterval:      5 * time.Minute,
		InitialDiscoveryDelay:  10 * time.Second,
		MetricsExportInterval:  30 * time.Second,
		PersistenceInterval:    1 * time.Minute,
		MaxConsecutiveFailures: 100,
		MinRetryInterval:       1 * time.Second,
		MaxRetryInterval:       30 * time.Second,
	}
}

// ParentRef represents a reference to a parent resource, including UID.
type ParentRef struct {
	kube.ResourceKey
	UID string
}

// ResourceMetadata contains lightweight metadata about a resource.
type ResourceMetadata struct {
	Labels      map[string]string
	Annotations map[string]string
	OwnerRefs   []metav1.OwnerReference
}

// ResourceNode represents a single Kubernetes resource in the graph.
// It contains the resource identity, tracking information, and graph relationships.
type ResourceNode struct {
	// Core identity
	Key             kube.ResourceKey
	Version         string // API Version
	UID             string
	ResourceVersion string

	// Tracking information
	ManagedBy  string // Application name from tracking label/annotation
	TrackingID string // Full tracking ID (e.g., "app:group/kind:ns/name")

	// Graph relationships
	Parents  []ParentRef        // Resources that own this one (from OwnerReferences)
	Children []kube.ResourceKey // Resources owned by this one

	// Metadata replacement for full object
	Info *ResourceMetadata

	// Full resource object (for cache hits without API calls)
	// This increases memory usage but prevents unnecessary API calls
	Resource *unstructured.Unstructured

	// Metadata
	CreatedAt time.Time
	UpdatedAt time.Time
}

// GraphShard represents a partition of the resource graph to reduce lock contention
type GraphShard struct {
	nodes      map[kube.ResourceKey]*ResourceNode
	appIndex   map[string]map[kube.ResourceKey]bool
	typeIndex  map[schema.GroupKind]map[kube.ResourceKey]bool
	labelIndex map[string]map[string]map[kube.ResourceKey]bool // LabelKey -> LabelValue -> Set of ResourceKeys
	lock       sync.RWMutex
}

// ResourceGraph is the core graph structure storing all managed resources.
// It provides efficient lookups by resource key, application, and type.
type ResourceGraph struct {
	shards     []*GraphShard
	shardCount int
}

// NewResourceGraph creates a new empty resource graph with the specified shard count.
// If shardCount is 0 or negative, it defaults to 32.
func NewResourceGraph(shardCount int) *ResourceGraph {
	if shardCount <= 0 {
		shardCount = 32
	}

	g := &ResourceGraph{
		shards:     make([]*GraphShard, shardCount),
		shardCount: shardCount,
	}

	for i := 0; i < shardCount; i++ {
		g.shards[i] = &GraphShard{
			nodes:      make(map[kube.ResourceKey]*ResourceNode),
			appIndex:   make(map[string]map[kube.ResourceKey]bool),
			typeIndex:  make(map[schema.GroupKind]map[kube.ResourceKey]bool),
			labelIndex: make(map[string]map[string]map[kube.ResourceKey]bool),
		}
	}
	return g
}

func (g *ResourceGraph) getShard(key kube.ResourceKey) *GraphShard {
	h := fnv.New32a()
	h.Write([]byte(key.String()))
	index := h.Sum32() % uint32(g.shardCount)
	return g.shards[index]
}

// AddOrUpdate adds a new resource node to the graph or updates an existing one.
// It maintains all indices and relationships.
func (g *ResourceGraph) AddOrUpdate(node *ResourceNode) {
	shard := g.getShard(node.Key)

	// Update node and indices in its shard
	shard.lock.Lock()

	var oldParents []ParentRef
	var oldLabels map[string]string

	existing, exists := shard.nodes[node.Key]
	if exists {
		node.CreatedAt = existing.CreatedAt
		node.UpdatedAt = time.Now()
		// Preserve children from existing node
		node.Children = existing.Children
		// Capture old parents
		oldParents = make([]ParentRef, len(existing.Parents))
		copy(oldParents, existing.Parents)
		// Capture old labels
		if existing.Info != nil {
			oldLabels = existing.Info.Labels
		}
	} else {
		node.CreatedAt = time.Now()
		node.UpdatedAt = time.Now()
	}

	shard.nodes[node.Key] = node

	// Update App Index
	if node.ManagedBy != "" {
		if shard.appIndex[node.ManagedBy] == nil {
			shard.appIndex[node.ManagedBy] = make(map[kube.ResourceKey]bool)
		}
		shard.appIndex[node.ManagedBy][node.Key] = true
	}

	// Update Type Index
	gk := schema.GroupKind{Group: node.Key.Group, Kind: node.Key.Kind}
	if shard.typeIndex[gk] == nil {
		shard.typeIndex[gk] = make(map[kube.ResourceKey]bool)
	}
	shard.typeIndex[gk][node.Key] = true

	// Handle App Index cleanup (if app changed)
	if exists && existing.ManagedBy != "" && existing.ManagedBy != node.ManagedBy {
		if shard.appIndex[existing.ManagedBy] != nil {
			delete(shard.appIndex[existing.ManagedBy], node.Key)
			if len(shard.appIndex[existing.ManagedBy]) == 0 {
				delete(shard.appIndex, existing.ManagedBy)
			}
		}
	}

	// Update Label Index (optimized: only update changed labels)
	// Calculate delta (only changed labels)
	var toRemove []struct{ key, value string }
	var toAdd []struct{ key, value string }

	// Find labels to remove (present in old but not in new, or value changed)
	if oldLabels != nil {
		for k, oldVal := range oldLabels {
			if node.Info == nil || node.Info.Labels == nil {
				// All old labels should be removed
				toRemove = append(toRemove, struct{ key, value string }{k, oldVal})
			} else if newVal, exists := node.Info.Labels[k]; !exists || newVal != oldVal {
				// Label removed or value changed
				toRemove = append(toRemove, struct{ key, value string }{k, oldVal})
			}
		}
	}

	// Find labels to add (present in new but not in old, or value changed)
	if node.Info != nil && node.Info.Labels != nil {
		for k, newVal := range node.Info.Labels {
			if oldLabels == nil {
				// All new labels should be added
				toAdd = append(toAdd, struct{ key, value string }{k, newVal})
			} else if oldVal, exists := oldLabels[k]; !exists || oldVal != newVal {
				// Label added or value changed
				toAdd = append(toAdd, struct{ key, value string }{k, newVal})
			}
		}
	}

	// Only update changed labels (reduces lock contention and map operations)
	for _, label := range toRemove {
		if shard.labelIndex[label.key] != nil && shard.labelIndex[label.key][label.value] != nil {
			delete(shard.labelIndex[label.key][label.value], node.Key)
			if len(shard.labelIndex[label.key][label.value]) == 0 {
				delete(shard.labelIndex[label.key], label.value)
			}
			if len(shard.labelIndex[label.key]) == 0 {
				delete(shard.labelIndex, label.key)
			}
		}
	}

	for _, label := range toAdd {
		if shard.labelIndex[label.key] == nil {
			shard.labelIndex[label.key] = make(map[string]map[kube.ResourceKey]bool)
		}
		if shard.labelIndex[label.key][label.value] == nil {
			shard.labelIndex[label.key][label.value] = make(map[kube.ResourceKey]bool)
		}
		shard.labelIndex[label.key][label.value][node.Key] = true
	}

	shard.lock.Unlock()

	// Update relationships (cross-shard)

	// Calculate added and removed parents
	newParentsMap := make(map[kube.ResourceKey]bool)
	for _, p := range node.Parents {
		newParentsMap[p.ResourceKey] = true
	}

	oldParentsMap := make(map[kube.ResourceKey]bool)
	for _, p := range oldParents {
		oldParentsMap[p.ResourceKey] = true
	}

	// Add to new parents
	for _, parentRef := range node.Parents {
		if !oldParentsMap[parentRef.ResourceKey] {
			g.addChildToParent(parentRef.ResourceKey, node.Key)
		}
	}

	// Remove from old parents
	for _, parentRef := range oldParents {
		if !newParentsMap[parentRef.ResourceKey] {
			g.removeChildFromParent(parentRef.ResourceKey, node.Key)
		}
	}
}

func (g *ResourceGraph) addChildToParent(parentKey, childKey kube.ResourceKey) {
	shard := g.getShard(parentKey)
	shard.lock.Lock()
	defer shard.lock.Unlock()

	parent, exists := shard.nodes[parentKey]
	if exists {
		// Check if already present
		for _, c := range parent.Children {
			if c == childKey {
				return
			}
		}
		parent.Children = append(parent.Children, childKey)
	}
}

// Get retrieves a resource node by its key.
func (g *ResourceGraph) Get(key kube.ResourceKey) (*ResourceNode, bool) {
	shard := g.getShard(key)
	shard.lock.RLock()
	defer shard.lock.RUnlock()

	node, exists := shard.nodes[key]
	return node, exists
}

// Delete removes a resource node from the graph and all indices.
func (g *ResourceGraph) Delete(key kube.ResourceKey) {
	shard := g.getShard(key)
	shard.lock.Lock()

	node, exists := shard.nodes[key]
	if !exists {
		shard.lock.Unlock()
		return
	}

	// Remove from nodes
	delete(shard.nodes, key)

	// Remove from App Index
	if node.ManagedBy != "" {
		if shard.appIndex[node.ManagedBy] != nil {
			delete(shard.appIndex[node.ManagedBy], key)
			if len(shard.appIndex[node.ManagedBy]) == 0 {
				delete(shard.appIndex, node.ManagedBy)
			}
		}
	}

	// Remove from Type Index
	gk := schema.GroupKind{Group: node.Key.Group, Kind: node.Key.Kind}
	if shard.typeIndex[gk] != nil {
		delete(shard.typeIndex[gk], key)
		if len(shard.typeIndex[gk]) == 0 {
			delete(shard.typeIndex, gk)
		}
	}

	// Remove from Label Index
	if node.Info != nil && node.Info.Labels != nil {
		for k, v := range node.Info.Labels {
			if shard.labelIndex[k] != nil && shard.labelIndex[k][v] != nil {
				delete(shard.labelIndex[k][v], key)
				if len(shard.labelIndex[k][v]) == 0 {
					delete(shard.labelIndex[k], v)
				}
			}
		}
	}

	// Capture relationships to clean up
	parents := make([]ParentRef, len(node.Parents))
	copy(parents, node.Parents)
	children := make([]kube.ResourceKey, len(node.Children))
	copy(children, node.Children)

	shard.lock.Unlock()

	// Clean up relationships (cross-shard)
	for _, parentRef := range parents {
		g.removeChildFromParent(parentRef.ResourceKey, key)
	}
	for _, childKey := range children {
		g.removeParentFromChild(childKey, key)
	}
}

func (g *ResourceGraph) removeChildFromParent(parentKey, childKey kube.ResourceKey) {
	shard := g.getShard(parentKey)
	shard.lock.Lock()
	defer shard.lock.Unlock()

	parent, exists := shard.nodes[parentKey]
	if exists {
		for i, c := range parent.Children {
			if c == childKey {
				parent.Children = append(parent.Children[:i], parent.Children[i+1:]...)
				return
			}
		}
	}
}

func (g *ResourceGraph) removeParentFromChild(childKey, parentKey kube.ResourceKey) {
	shard := g.getShard(childKey)
	shard.lock.Lock()
	defer shard.lock.Unlock()

	child, exists := shard.nodes[childKey]
	if exists {
		for i, p := range child.Parents {
			if p.ResourceKey == parentKey {
				child.Parents = append(child.Parents[:i], child.Parents[i+1:]...)
				return
			}
		}
	}
}

// GetByApplication returns all resources managed by a specific application.
func (g *ResourceGraph) GetByApplication(appName string) []*ResourceNode {
	var nodes []*ResourceNode

	// Gather from all shards
	for _, shard := range g.shards {
		shard.lock.RLock()
		if keys, ok := shard.appIndex[appName]; ok {
			for key := range keys {
				if node, exists := shard.nodes[key]; exists {
					nodes = append(nodes, node)
				}
			}
		}
		shard.lock.RUnlock()
	}

	return nodes
}

// GetByType returns all resources of a specific GroupKind.
func (g *ResourceGraph) GetByType(gk schema.GroupKind) []*ResourceNode {
	var nodes []*ResourceNode

	for _, shard := range g.shards {
		shard.lock.RLock()
		if keys, ok := shard.typeIndex[gk]; ok {
			for key := range keys {
				if node, exists := shard.nodes[key]; exists {
					nodes = append(nodes, node)
				}
			}
		}
		shard.lock.RUnlock()
	}

	return nodes
}

// GetByLabel returns all resources matching a label key and value.
func (g *ResourceGraph) GetByLabel(key, value string) []*ResourceNode {
	var nodes []*ResourceNode

	for _, shard := range g.shards {
		shard.lock.RLock()
		if values, ok := shard.labelIndex[key]; ok {
			if keys, ok := values[value]; ok {
				for k := range keys {
					if node, exists := shard.nodes[k]; exists {
						nodes = append(nodes, node)
					}
				}
			}
		}
		shard.lock.RUnlock()
	}

	return nodes
}

// GetChildren returns all direct children of a resource.
func (g *ResourceGraph) GetChildren(key kube.ResourceKey) []*ResourceNode {
	shard := g.getShard(key)
	shard.lock.RLock()

	node, exists := shard.nodes[key]
	if !exists {
		shard.lock.RUnlock()
		return nil
	}

	childKeys := make([]kube.ResourceKey, len(node.Children))
	copy(childKeys, node.Children)
	shard.lock.RUnlock()

	// Children might be in different shards
	var children []*ResourceNode
	for _, childKey := range childKeys {
		if child, found := g.Get(childKey); found {
			children = append(children, child)
		}
	}

	return children
}

// GetParents returns all direct parents of a resource.
func (g *ResourceGraph) GetParents(key kube.ResourceKey) []*ResourceNode {
	shard := g.getShard(key)
	shard.lock.RLock()

	node, exists := shard.nodes[key]
	if !exists {
		shard.lock.RUnlock()
		return nil
	}

	parentRefs := make([]ParentRef, len(node.Parents))
	copy(parentRefs, node.Parents)
	shard.lock.RUnlock()

	var parents []*ResourceNode
	for _, parentRef := range parentRefs {
		if parent, found := g.Get(parentRef.ResourceKey); found {
			parents = append(parents, parent)
		}
	}

	return parents
}

// GetAllTypes returns all GroupKinds currently in the graph.
func (g *ResourceGraph) GetAllTypes() []schema.GroupKind {
	typeMap := make(map[schema.GroupKind]bool)

	for _, shard := range g.shards {
		shard.lock.RLock()
		for gk := range shard.typeIndex {
			typeMap[gk] = true
		}
		shard.lock.RUnlock()
	}

	types := make([]schema.GroupKind, 0, len(typeMap))
	for gk := range typeMap {
		types = append(types, gk)
	}

	return types
}

// GetAllNodes returns all nodes in the graph.
func (g *ResourceGraph) GetAllNodes() []*ResourceNode {
	var nodes []*ResourceNode

	for _, shard := range g.shards {
		shard.lock.RLock()
		for _, node := range shard.nodes {
			nodes = append(nodes, node)
		}
		shard.lock.RUnlock()
	}

	return nodes
}

// GetAllApplications returns all application names currently in the graph.
func (g *ResourceGraph) GetAllApplications() []string {
	appMap := make(map[string]bool)

	for _, shard := range g.shards {
		shard.lock.RLock()
		for app := range shard.appIndex {
			appMap[app] = true
		}
		shard.lock.RUnlock()
	}

	apps := make([]string, 0, len(appMap))
	for app := range appMap {
		apps = append(apps, app)
	}

	return apps
}

// Size returns the total number of resources in the graph.
func (g *ResourceGraph) Size() int {
	total := 0
	for _, shard := range g.shards {
		shard.lock.RLock()
		total += len(shard.nodes)
		shard.lock.RUnlock()
	}
	return total
}

// GetMetrics returns statistics about the graph for monitoring.
func (g *ResourceGraph) GetMetrics() GraphMetrics {
	metrics := GraphMetrics{
		ResourcesByType: make(map[schema.GroupKind]int),
		ResourcesByApp:  make(map[string]int),
	}

	uniqueApps := make(map[string]bool)
	uniqueTypes := make(map[schema.GroupKind]bool)

	for _, shard := range g.shards {
		shard.lock.RLock()
		metrics.TotalResources += len(shard.nodes)

		for gk, keys := range shard.typeIndex {
			metrics.ResourcesByType[gk] += len(keys)
			uniqueTypes[gk] = true
		}

		for app, keys := range shard.appIndex {
			metrics.ResourcesByApp[app] += len(keys)
			uniqueApps[app] = true
		}
		shard.lock.RUnlock()
	}

	metrics.UniqueApplications = len(uniqueApps)
	metrics.UniqueResourceTypes = len(uniqueTypes)

	return metrics
}

// GraphMetrics contains statistics about the resource graph.
type GraphMetrics struct {
	TotalResources      int
	ResourcesByType     map[schema.GroupKind]int
	ResourcesByApp      map[string]int
	UniqueApplications  int
	UniqueResourceTypes int
}
