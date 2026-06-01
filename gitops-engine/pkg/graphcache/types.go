package graphcache

import (
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
)

// GraphConfig contains tunable parameters for the graph cache.
type GraphConfig struct {
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
func DefaultGraphConfig() GraphConfig {
	return GraphConfig{
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
	UID    string
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
	Parents  []ParentRef       // Resources that own this one (from OwnerReferences)
	Children []kube.ResourceKey // Resources owned by this one

	// Metadata replacement for full object
	Info *ResourceMetadata

	// Full resource object (for cache hits without API calls)
	Resource *unstructured.Unstructured

	// CachedInfo stores the result of PopulateResourceInfoHandler.
	// Type-assert to *controller/cache.ResourceInfo to access health, images, etc.
	CachedInfo interface{}

	// Metadata
	CreatedAt time.Time
	UpdatedAt time.Time
}

// shallowCopy returns a shallow copy of the ResourceNode.
// Scalar and pointer fields are copied by value so callers cannot mutate the
// live graph node. Slice fields (Parents, Children) get their own backing
// arrays to prevent append-aliasing.
func (n *ResourceNode) shallowCopy() *ResourceNode {
	copied := *n
	if len(n.Parents) > 0 {
		copied.Parents = make([]ParentRef, len(n.Parents))
		copy(copied.Parents, n.Parents)
	}
	if len(n.Children) > 0 {
		copied.Children = make([]kube.ResourceKey, len(n.Children))
		copy(copied.Children, n.Children)
	}
	return &copied
}

// ResourceGraph is the core graph structure storing all managed resources.
// It provides efficient lookups by resource key, application, type, namespace, and UID.
// All indices are protected by a single RWMutex.
type ResourceGraph struct {
	mu         sync.RWMutex
	nodes      map[kube.ResourceKey]*ResourceNode
	appIndex   map[string]map[kube.ResourceKey]bool
	typeIndex  map[schema.GroupKind]map[kube.ResourceKey]bool
	labelIndex map[string]map[string]map[kube.ResourceKey]bool // LabelKey -> LabelValue -> Set of ResourceKeys
	nsIndex    map[string]map[kube.ResourceKey]bool            // namespace -> set of ResourceKeys
	uidIndex   map[string]kube.ResourceKey                     // UID -> ResourceKey
}

// NewResourceGraph creates a new empty resource graph.
func NewResourceGraph() *ResourceGraph {
	return &ResourceGraph{
		nodes:      make(map[kube.ResourceKey]*ResourceNode),
		appIndex:   make(map[string]map[kube.ResourceKey]bool),
		typeIndex:  make(map[schema.GroupKind]map[kube.ResourceKey]bool),
		labelIndex: make(map[string]map[string]map[kube.ResourceKey]bool),
		nsIndex:    make(map[string]map[kube.ResourceKey]bool),
		uidIndex:   make(map[string]kube.ResourceKey),
	}
}

// AddOrUpdate adds a new resource node to the graph or updates an existing one.
// It maintains all indices and relationships.
func (g *ResourceGraph) AddOrUpdate(node *ResourceNode) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var oldParents []ParentRef
	var oldLabels map[string]string

	existing, exists := g.nodes[node.Key]
	if exists {
		node.CreatedAt = existing.CreatedAt
		node.UpdatedAt = time.Now()
		node.Children = existing.Children
		oldParents = make([]ParentRef, len(existing.Parents))
		copy(oldParents, existing.Parents)
		// Clean up stale UID index entry
		if existing.UID != "" && existing.UID != node.UID {
			delete(g.uidIndex, existing.UID)
		}
		if existing.Info != nil {
			oldLabels = existing.Info.Labels
		}
	} else {
		node.CreatedAt = time.Now()
		node.UpdatedAt = time.Now()
	}

	g.nodes[node.Key] = node

	// Update UID index
	if node.UID != "" {
		g.uidIndex[node.UID] = node.Key
	}

	// Update App Index
	if node.ManagedBy != "" {
		if g.appIndex[node.ManagedBy] == nil {
			g.appIndex[node.ManagedBy] = make(map[kube.ResourceKey]bool)
		}
		g.appIndex[node.ManagedBy][node.Key] = true
	}

	// Update Type Index
	gk := schema.GroupKind{Group: node.Key.Group, Kind: node.Key.Kind}
	if g.typeIndex[gk] == nil {
		g.typeIndex[gk] = make(map[kube.ResourceKey]bool)
	}
	g.typeIndex[gk][node.Key] = true

	// Update Namespace Index
	if node.Key.Namespace != "" {
		if g.nsIndex[node.Key.Namespace] == nil {
			g.nsIndex[node.Key.Namespace] = make(map[kube.ResourceKey]bool)
		}
		g.nsIndex[node.Key.Namespace][node.Key] = true
	}

	// Handle App Index cleanup (if app changed)
	if exists && existing.ManagedBy != "" && existing.ManagedBy != node.ManagedBy {
		if g.appIndex[existing.ManagedBy] != nil {
			delete(g.appIndex[existing.ManagedBy], node.Key)
			if len(g.appIndex[existing.ManagedBy]) == 0 {
				delete(g.appIndex, existing.ManagedBy)
			}
		}
	}

	// Update Label Index (only changed labels)
	var toRemove []struct{ key, value string }
	var toAdd []struct{ key, value string }

	if oldLabels != nil {
		for k, oldVal := range oldLabels {
			if node.Info == nil || node.Info.Labels == nil {
				toRemove = append(toRemove, struct{ key, value string }{k, oldVal})
			} else if newVal, exists := node.Info.Labels[k]; !exists || newVal != oldVal {
				toRemove = append(toRemove, struct{ key, value string }{k, oldVal})
			}
		}
	}

	if node.Info != nil && node.Info.Labels != nil {
		for k, newVal := range node.Info.Labels {
			if oldLabels == nil {
				toAdd = append(toAdd, struct{ key, value string }{k, newVal})
			} else if oldVal, exists := oldLabels[k]; !exists || oldVal != newVal {
				toAdd = append(toAdd, struct{ key, value string }{k, newVal})
			}
		}
	}

	for _, label := range toRemove {
		if g.labelIndex[label.key] != nil && g.labelIndex[label.key][label.value] != nil {
			delete(g.labelIndex[label.key][label.value], node.Key)
			if len(g.labelIndex[label.key][label.value]) == 0 {
				delete(g.labelIndex[label.key], label.value)
			}
			if len(g.labelIndex[label.key]) == 0 {
				delete(g.labelIndex, label.key)
			}
		}
	}

	for _, label := range toAdd {
		if g.labelIndex[label.key] == nil {
			g.labelIndex[label.key] = make(map[string]map[kube.ResourceKey]bool)
		}
		if g.labelIndex[label.key][label.value] == nil {
			g.labelIndex[label.key][label.value] = make(map[kube.ResourceKey]bool)
		}
		g.labelIndex[label.key][label.value][node.Key] = true
	}

	// Update relationships
	newParentsMap := make(map[kube.ResourceKey]bool)
	for _, p := range node.Parents {
		newParentsMap[p.ResourceKey] = true
	}

	oldParentsMap := make(map[kube.ResourceKey]bool)
	for _, p := range oldParents {
		oldParentsMap[p.ResourceKey] = true
	}

	for _, parentRef := range node.Parents {
		if !oldParentsMap[parentRef.ResourceKey] {
			g.addChildToParent(parentRef.ResourceKey, node.Key)
		}
	}

	for _, parentRef := range oldParents {
		if !newParentsMap[parentRef.ResourceKey] {
			g.removeChildFromParent(parentRef.ResourceKey, node.Key)
		}
	}
}

// addChildToParent adds childKey to parent's Children list. Caller must hold g.mu.
func (g *ResourceGraph) addChildToParent(parentKey, childKey kube.ResourceKey) {
	parent, exists := g.nodes[parentKey]
	if !exists {
		return
	}
	for _, c := range parent.Children {
		if c == childKey {
			return
		}
	}
	parent.Children = append(parent.Children, childKey)
}

// Get retrieves a shallow copy of a resource node by its key.
func (g *ResourceGraph) Get(key kube.ResourceKey) (*ResourceNode, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	node, exists := g.nodes[key]
	if !exists {
		return nil, false
	}
	return node.shallowCopy(), true
}

// Delete removes a resource node from the graph and all indices.
func (g *ResourceGraph) Delete(key kube.ResourceKey) {
	g.mu.Lock()
	defer g.mu.Unlock()

	node, exists := g.nodes[key]
	if !exists {
		return
	}

	delete(g.nodes, key)

	// Remove from UID index
	if node.UID != "" {
		delete(g.uidIndex, node.UID)
	}

	// Remove from App Index
	if node.ManagedBy != "" {
		if g.appIndex[node.ManagedBy] != nil {
			delete(g.appIndex[node.ManagedBy], key)
			if len(g.appIndex[node.ManagedBy]) == 0 {
				delete(g.appIndex, node.ManagedBy)
			}
		}
	}

	// Remove from Type Index
	gk := schema.GroupKind{Group: node.Key.Group, Kind: node.Key.Kind}
	if g.typeIndex[gk] != nil {
		delete(g.typeIndex[gk], key)
		if len(g.typeIndex[gk]) == 0 {
			delete(g.typeIndex, gk)
		}
	}

	// Remove from Namespace Index
	if key.Namespace != "" {
		if g.nsIndex[key.Namespace] != nil {
			delete(g.nsIndex[key.Namespace], key)
			if len(g.nsIndex[key.Namespace]) == 0 {
				delete(g.nsIndex, key.Namespace)
			}
		}
	}

	// Remove from Label Index
	if node.Info != nil && node.Info.Labels != nil {
		for k, v := range node.Info.Labels {
			if g.labelIndex[k] != nil && g.labelIndex[k][v] != nil {
				delete(g.labelIndex[k][v], key)
				if len(g.labelIndex[k][v]) == 0 {
					delete(g.labelIndex[k], v)
				}
			}
		}
	}

	// Clean up relationships
	for _, parentRef := range node.Parents {
		g.removeChildFromParent(parentRef.ResourceKey, key)
	}
	for _, childKey := range node.Children {
		g.removeParentFromChild(childKey, key)
	}
}

// removeChildFromParent removes childKey from parent's Children list. Caller must hold g.mu.
func (g *ResourceGraph) removeChildFromParent(parentKey, childKey kube.ResourceKey) {
	parent, exists := g.nodes[parentKey]
	if !exists {
		return
	}
	for i, c := range parent.Children {
		if c == childKey {
			parent.Children = append(parent.Children[:i], parent.Children[i+1:]...)
			return
		}
	}
}

// removeParentFromChild removes parentKey from child's Parents list. Caller must hold g.mu.
func (g *ResourceGraph) removeParentFromChild(childKey, parentKey kube.ResourceKey) {
	child, exists := g.nodes[childKey]
	if !exists {
		return
	}
	for i, p := range child.Parents {
		if p.ResourceKey == parentKey {
			child.Parents = append(child.Parents[:i], child.Parents[i+1:]...)
			return
		}
	}
}

// GetByUID looks up a resource by its UID. Used for cross-namespace parent lookups.
func (g *ResourceGraph) GetByUID(uid string) (*ResourceNode, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	key, exists := g.uidIndex[uid]
	if !exists {
		return nil, false
	}
	node, exists := g.nodes[key]
	if !exists {
		return nil, false
	}
	return node.shallowCopy(), true
}

// GetByApplication returns shallow copies of all resources managed by a specific application.
func (g *ResourceGraph) GetByApplication(appName string) []*ResourceNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	keys, ok := g.appIndex[appName]
	if !ok {
		return nil
	}

	nodes := make([]*ResourceNode, 0, len(keys))
	for key := range keys {
		if node, exists := g.nodes[key]; exists {
			nodes = append(nodes, node.shallowCopy())
		}
	}
	return nodes
}

// GetByType returns shallow copies of all resources of a specific GroupKind.
func (g *ResourceGraph) GetByType(gk schema.GroupKind) []*ResourceNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	keys, ok := g.typeIndex[gk]
	if !ok {
		return nil
	}

	nodes := make([]*ResourceNode, 0, len(keys))
	for key := range keys {
		if node, exists := g.nodes[key]; exists {
			nodes = append(nodes, node.shallowCopy())
		}
	}
	return nodes
}

// GetByLabel returns shallow copies of all resources matching a label key and value.
func (g *ResourceGraph) GetByLabel(key, value string) []*ResourceNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	values, ok := g.labelIndex[key]
	if !ok {
		return nil
	}
	keys, ok := values[value]
	if !ok {
		return nil
	}

	nodes := make([]*ResourceNode, 0, len(keys))
	for k := range keys {
		if node, exists := g.nodes[k]; exists {
			nodes = append(nodes, node.shallowCopy())
		}
	}
	return nodes
}

// GetByNamespace returns shallow copies of all resources in a specific namespace.
func (g *ResourceGraph) GetByNamespace(namespace string) []*ResourceNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	keys, ok := g.nsIndex[namespace]
	if !ok {
		return nil
	}

	nodes := make([]*ResourceNode, 0, len(keys))
	for key := range keys {
		if node, exists := g.nodes[key]; exists {
			nodes = append(nodes, node.shallowCopy())
		}
	}
	return nodes
}

// GetChildren returns all direct children of a resource.
func (g *ResourceGraph) GetChildren(key kube.ResourceKey) []*ResourceNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	node, exists := g.nodes[key]
	if !exists {
		return nil
	}

	children := make([]*ResourceNode, 0, len(node.Children))
	for _, childKey := range node.Children {
		if child, found := g.nodes[childKey]; found {
			children = append(children, child.shallowCopy())
		}
	}
	return children
}

// GetParents returns all direct parents of a resource.
func (g *ResourceGraph) GetParents(key kube.ResourceKey) []*ResourceNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	node, exists := g.nodes[key]
	if !exists {
		return nil
	}

	parents := make([]*ResourceNode, 0, len(node.Parents))
	for _, parentRef := range node.Parents {
		if parent, found := g.nodes[parentRef.ResourceKey]; found {
			parents = append(parents, parent.shallowCopy())
		}
	}
	return parents
}

// GetAllTypes returns all GroupKinds currently in the graph.
func (g *ResourceGraph) GetAllTypes() []schema.GroupKind {
	g.mu.RLock()
	defer g.mu.RUnlock()

	types := make([]schema.GroupKind, 0, len(g.typeIndex))
	for gk := range g.typeIndex {
		types = append(types, gk)
	}
	return types
}

// GetAllNodes returns shallow copies of all nodes in the graph.
func (g *ResourceGraph) GetAllNodes() []*ResourceNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	nodes := make([]*ResourceNode, 0, len(g.nodes))
	for _, node := range g.nodes {
		nodes = append(nodes, node.shallowCopy())
	}
	return nodes
}

// GetAllApplications returns all application names currently in the graph.
func (g *ResourceGraph) GetAllApplications() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	apps := make([]string, 0, len(g.appIndex))
	for app := range g.appIndex {
		apps = append(apps, app)
	}
	return apps
}

// Size returns the total number of resources in the graph.
func (g *ResourceGraph) Size() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes)
}

// GetMetrics returns statistics about the graph for monitoring.
func (g *ResourceGraph) GetMetrics() GraphMetrics {
	g.mu.RLock()
	defer g.mu.RUnlock()

	metrics := GraphMetrics{
		TotalResources:      len(g.nodes),
		ResourcesByType:     make(map[schema.GroupKind]int, len(g.typeIndex)),
		ResourcesByApp:      make(map[string]int, len(g.appIndex)),
		UniqueApplications:  len(g.appIndex),
		UniqueResourceTypes: len(g.typeIndex),
	}

	for gk, keys := range g.typeIndex {
		metrics.ResourcesByType[gk] = len(keys)
	}

	for app, keys := range g.appIndex {
		metrics.ResourcesByApp[app] = len(keys)
	}

	return metrics
}

// UpdateNodeAppName updates the ManagedBy field for a node and fixes the app index.
func (g *ResourceGraph) UpdateNodeAppName(key kube.ResourceKey, appName string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	node, exists := g.nodes[key]
	if !exists {
		return
	}

	oldApp := node.ManagedBy
	if oldApp == appName {
		return
	}

	// Remove from old app index
	if oldApp != "" {
		if g.appIndex[oldApp] != nil {
			delete(g.appIndex[oldApp], key)
			if len(g.appIndex[oldApp]) == 0 {
				delete(g.appIndex, oldApp)
			}
		}
	}

	node.ManagedBy = appName

	// Add to new app index
	if appName != "" {
		if g.appIndex[appName] == nil {
			g.appIndex[appName] = make(map[kube.ResourceKey]bool)
		}
		g.appIndex[appName][key] = true
	}
}

// GraphMetrics contains statistics about the resource graph.
type GraphMetrics struct {
	TotalResources      int
	ResourcesByType     map[schema.GroupKind]int
	ResourcesByApp      map[string]int
	UniqueApplications  int
	UniqueResourceTypes int
}
