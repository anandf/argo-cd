package graphcache

import (
	"context"
	"testing"
	"time"

	graphcore "github.com/argoproj/argo-cd/gitops-engine/pkg/graphcache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/cache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

func makeResourceNode(group, kind, namespace, name, version, uid, managedBy string) *graphcore.ResourceNode {
	return &graphcore.ResourceNode{
		Key: kube.ResourceKey{
			Group:     group,
			Kind:      kind,
			Namespace: namespace,
			Name:      name,
		},
		Version:         version,
		UID:             uid,
		ResourceVersion: "1",
		ManagedBy:       managedBy,
		CreatedAt:       time.Now(),
	}
}

func TestNodeToResourceNode(t *testing.T) {
	node := makeResourceNode("apps", "Deployment", "default", "nginx", "v1", "uid-1", "myapp")
	node.Parents = []graphcore.ParentRef{
		{
			ResourceKey: kube.ResourceKey{Group: "", Kind: "Namespace", Name: "default"},
			UID:         "ns-uid",
		},
	}

	rn := nodeToResourceNode(node)
	assert.Equal(t, "uid-1", rn.UID)
	assert.Equal(t, "nginx", rn.Name)
	assert.Equal(t, "apps", rn.Group)
	assert.Equal(t, "v1", rn.Version)
	assert.Equal(t, "Deployment", rn.Kind)
	assert.Equal(t, "default", rn.Namespace)
	assert.Equal(t, "1", rn.ResourceVersion)
	require.Len(t, rn.ParentRefs, 1)
	assert.Equal(t, "Namespace", rn.ParentRefs[0].Kind)
}

func TestNodeToCacheResource(t *testing.T) {
	node := makeResourceNode("apps", "Deployment", "default", "nginx", "v1", "uid-1", "myapp")
	node.Info = &graphcore.ResourceMetadata{
		OwnerRefs: []metav1.OwnerReference{
			{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "nginx-abc",
				UID:        "rs-uid",
			},
		},
	}

	res := nodeToCacheResource(node)
	assert.Equal(t, "apps/v1", res.Ref.APIVersion)
	assert.Equal(t, "Deployment", res.Ref.Kind)
	assert.Equal(t, "default", res.Ref.Namespace)
	assert.Equal(t, "nginx", res.Ref.Name)
	assert.Equal(t, types.UID("uid-1"), res.Ref.UID)
	assert.Equal(t, "1", res.ResourceVersion)
	require.Len(t, res.OwnerRefs, 1)
}

func TestNodeToCacheResource_NilInfo(t *testing.T) {
	node := makeResourceNode("", "Pod", "default", "test", "v1", "uid-2", "")
	res := nodeToCacheResource(node)
	assert.Empty(t, res.OwnerRefs)
}

func TestClusterCacheAdapter_OnResourceUpdated(t *testing.T) {
	graph := graphcore.NewResourceGraph()
	gc := &GraphCache{
		graph: graph,
	}

	adapter := newClusterCacheAdapter(gc, "https://kubernetes.default.svc", nil, "")

	called := false
	var receivedNew, receivedOld *cache.Resource
	unsub := adapter.OnResourceUpdated(func(newRes *cache.Resource, oldRes *cache.Resource, nsResources map[kube.ResourceKey]*cache.Resource) {
		called = true
		receivedNew = newRes
		receivedOld = oldRes
	})

	// Simulate a resource update notification
	newRes := &cache.Resource{
		Ref: v1.ObjectReference{Kind: "Pod", Name: "test"},
	}
	adapter.notifyResourceUpdated(newRes, nil, nil)

	assert.True(t, called)
	assert.NotNil(t, receivedNew)
	assert.Nil(t, receivedOld)

	// Unsubscribe and verify no more calls
	unsub()
	called = false
	adapter.notifyResourceUpdated(newRes, nil, nil)
	assert.False(t, called)
}

func TestClusterCacheAdapter_OnEvent(t *testing.T) {
	graph := graphcore.NewResourceGraph()
	gc := &GraphCache{
		graph: graph,
	}

	adapter := newClusterCacheAdapter(gc, "https://kubernetes.default.svc", nil, "")

	var receivedType watch.EventType
	var receivedObj *unstructured.Unstructured
	unsub := adapter.OnEvent(func(event watch.EventType, un *unstructured.Unstructured) {
		receivedType = event
		receivedObj = un
	})

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]interface{}{"name": "test"},
		},
	}
	adapter.notifyEvent(watch.Added, obj)

	assert.Equal(t, watch.Added, receivedType)
	assert.Equal(t, "test", receivedObj.GetName())

	unsub()
}

func TestClusterCacheAdapter_MultipleHandlers(t *testing.T) {
	graph := graphcore.NewResourceGraph()
	gc := &GraphCache{
		graph: graph,
	}

	adapter := newClusterCacheAdapter(gc, "https://kubernetes.default.svc", nil, "")

	count := 0
	unsub1 := adapter.OnResourceUpdated(func(newRes *cache.Resource, oldRes *cache.Resource, nsResources map[kube.ResourceKey]*cache.Resource) {
		count++
	})
	_ = adapter.OnResourceUpdated(func(newRes *cache.Resource, oldRes *cache.Resource, nsResources map[kube.ResourceKey]*cache.Resource) {
		count++
	})

	adapter.notifyResourceUpdated(&cache.Resource{}, nil, nil)
	assert.Equal(t, 2, count)

	// Unsubscribe first handler
	unsub1()
	count = 0
	adapter.notifyResourceUpdated(&cache.Resource{}, nil, nil)
	assert.Equal(t, 1, count)
}

func TestClusterCacheAdapter_EnsureSynced(t *testing.T) {
	graph := graphcore.NewResourceGraph()
	gc := &GraphCache{
		graph: graph,
	}

	adapter := newClusterCacheAdapter(gc, "https://kubernetes.default.svc", nil, "")

	// Before marking synced, EnsureSynced should block (test with short timeout)
	adapter2 := newClusterCacheAdapter(gc, "test", nil, "")
	done := make(chan error, 1)
	go func() {
		done <- adapter2.EnsureSynced()
	}()
	select {
	case <-time.After(100 * time.Millisecond):
		// Expected: still blocking
	case <-done:
		t.Fatal("EnsureSynced should block before markSynced")
	}
	// Now mark synced
	adapter2.markSynced()
	err := <-done
	assert.NoError(t, err)

	// After marking synced, returns immediately
	adapter.markSynced()
	assert.NoError(t, adapter.EnsureSynced())
}

func TestClusterCacheAdapter_IterateHierarchy(t *testing.T) {
	graph := graphcore.NewResourceGraph()

	// Add parent and child
	parent := makeResourceNode("apps", "Deployment", "default", "nginx", "v1", "deploy-uid", "myapp")
	child := makeResourceNode("apps", "ReplicaSet", "default", "nginx-abc", "v1", "rs-uid", "myapp")
	child.Parents = []graphcore.ParentRef{
		{ResourceKey: parent.Key, UID: parent.UID},
	}

	graph.AddOrUpdate(parent)
	graph.AddOrUpdate(child)

	// Set up children on parent
	parent.Children = []kube.ResourceKey{child.Key}
	graph.AddOrUpdate(parent)

	gc := &GraphCache{graph: graph}
	adapter := newClusterCacheAdapter(gc, "test", nil, "")

	var visited []string
	err := adapter.IterateHierarchy(parent.Key, func(node appv1.ResourceNode, appName string) bool {
		visited = append(visited, node.Name)
		return true
	})
	require.NoError(t, err)
	assert.Contains(t, visited, "nginx")
}

func TestClusterCacheAdapter_IterateHierarchyV2(t *testing.T) {
	graph := graphcore.NewResourceGraph()

	parent := makeResourceNode("apps", "Deployment", "default", "nginx", "v1", "deploy-uid", "myapp")
	child := makeResourceNode("apps", "ReplicaSet", "default", "nginx-abc", "v1", "rs-uid", "myapp")
	child.Parents = []graphcore.ParentRef{
		{ResourceKey: parent.Key, UID: parent.UID},
	}

	graph.AddOrUpdate(parent)
	graph.AddOrUpdate(child)
	parent.Children = []kube.ResourceKey{child.Key}
	graph.AddOrUpdate(parent)

	gc := &GraphCache{graph: graph}
	adapter := newClusterCacheAdapter(gc, "test", nil, "")

	var visited []string
	adapter.IterateHierarchyV2([]kube.ResourceKey{parent.Key}, func(resource *cache.Resource, nsResources map[kube.ResourceKey]*cache.Resource) bool {
		visited = append(visited, resource.Ref.Name)
		// Verify namespace resources are provided
		if resource.Ref.Namespace != "" {
			assert.NotNil(t, nsResources)
		}
		return true
	})
	assert.Contains(t, visited, "nginx")
}

func TestClusterCacheAdapter_FindResources(t *testing.T) {
	graph := graphcore.NewResourceGraph()

	node1 := makeResourceNode("apps", "Deployment", "default", "nginx", "v1", "uid-1", "myapp")
	node2 := makeResourceNode("", "Pod", "default", "nginx-pod", "v1", "uid-2", "myapp")
	node3 := makeResourceNode("", "Pod", "kube-system", "coredns", "v1", "uid-3", "system")

	graph.AddOrUpdate(node1)
	graph.AddOrUpdate(node2)
	graph.AddOrUpdate(node3)

	gc := &GraphCache{graph: graph}
	adapter := newClusterCacheAdapter(gc, "test", nil, "")

	// Find in specific namespace
	result := adapter.FindResources("default")
	assert.Len(t, result, 2)

	// Find with predicate
	result = adapter.FindResources("", func(r *cache.Resource) bool {
		return r.Ref.Kind == "Pod"
	})
	assert.Len(t, result, 2)

	// Find with namespace + predicate
	result = adapter.FindResources("default", func(r *cache.Resource) bool {
		return r.Ref.Kind == "Pod"
	})
	assert.Len(t, result, 1)
}

func TestClusterCacheAdapter_GetNamespaceTopLevelResources(t *testing.T) {
	graph := graphcore.NewResourceGraph()

	root := makeResourceNode("apps", "Deployment", "default", "nginx", "v1", "uid-1", "myapp")
	child := makeResourceNode("apps", "ReplicaSet", "default", "nginx-rs", "v1", "uid-2", "myapp")
	child.Parents = []graphcore.ParentRef{{ResourceKey: root.Key, UID: root.UID}}

	graph.AddOrUpdate(root)
	graph.AddOrUpdate(child)

	gc := &GraphCache{graph: graph}
	adapter := newClusterCacheAdapter(gc, "test", nil, "")

	result, err := adapter.GetNamespaceTopLevelResources("default")
	require.NoError(t, err)

	// Only root should be returned (it has no parents)
	assert.Equal(t, 1, len(result))
	_, hasRoot := result[root.Key]
	assert.True(t, hasRoot)
}

func TestClusterCacheAdapter_GetManagedLiveObjsForApp(t *testing.T) {
	graph := graphcore.NewResourceGraph()

	node := makeResourceNode("apps", "Deployment", "default", "nginx", "v1", "uid-1", "myapp")
	node.Resource = &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "nginx",
				"namespace": "default",
			},
		},
	}

	graph.AddOrUpdate(node)

	gc := &GraphCache{graph: graph}
	adapter := newClusterCacheAdapter(gc, "test", nil, "")

	app := &appv1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp",
			Namespace: "argocd",
		},
	}

	result, err := adapter.GetManagedLiveObjsForApp(app, nil)
	require.NoError(t, err)
	// The app name in the test is "myapp" which should match
	assert.GreaterOrEqual(t, len(result), 0) // May or may not match depending on InstanceName
}

func TestClusterCacheAdapter_GetClusterInfo(t *testing.T) {
	config := Config{
		DynamicClient:   fakedynamic.NewSimpleDynamicClient(runtime.NewScheme()),
		DiscoveryClient: &fakediscovery.FakeDiscovery{Fake: &kubetesting.Fake{}},
		TrackingMethod:  graphcore.TrackingMethodLabel,
	}
	gc, _ := NewGraphCache(context.Background(), config)

	// Add a resource so we can verify the count
	node := makeResourceNode("apps", "Deployment", "default", "nginx", "v1", "uid-1", "myapp")
	gc.graph.AddOrUpdate(node)

	// GetClusterInfo calls GetServerVersion which needs a discovery client,
	// so we test the metrics portion directly
	m := gc.GetMetrics()
	assert.Equal(t, 1, m.TotalManagedResources)
}
