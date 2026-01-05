package graphcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"
)

func TestNewGraphCache(t *testing.T) {
	scheme := runtime.NewScheme()
	dynamicClient := fakedynamic.NewSimpleDynamicClient(scheme)
	discoveryClient := &fakediscovery.FakeDiscovery{
		Fake: &kubetesting.Fake{},
	}

	config := Config{
		DynamicClient:   dynamicClient,
		DiscoveryClient: discoveryClient,
		TrackingMethod:  TrackingMethodLabel,
		Namespaces:      []string{"argocd"},
	}

	gc, err := NewGraphCache(config)
	assert.NoError(t, err)
	assert.NotNil(t, gc)
	assert.NotNil(t, gc.graph)
	assert.NotNil(t, gc.watchManager)
}

func TestGraphCache_AddResource(t *testing.T) {
	config := Config{
		DynamicClient:   fakedynamic.NewSimpleDynamicClient(runtime.NewScheme()),
		DiscoveryClient: &fakediscovery.FakeDiscovery{Fake: &kubetesting.Fake{}},
		TrackingMethod:  TrackingMethodLabel,
	}
	gc, _ := NewGraphCache(config)

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
				"labels": map[string]interface{}{
					"app.kubernetes.io/instance": "test-app",
				},
			},
		},
	}

	gc.addResourceToGraph(obj)

	// Verify it's in the graph
	key := ToResourceKey(obj)
	node, exists := gc.graph.Get(key)
	assert.True(t, exists)
	assert.Equal(t, "test-app", node.ManagedBy)
	
	// Verify indices
	appNodes := gc.GetResourcesByApplication("test-app")
	assert.Len(t, appNodes, 1)
	assert.Equal(t, key, appNodes[0].Key)
}

func TestGraphCache_EventHandling(t *testing.T) {
	config := Config{
		DynamicClient:   fakedynamic.NewSimpleDynamicClient(runtime.NewScheme()),
		DiscoveryClient: &fakediscovery.FakeDiscovery{Fake: &kubetesting.Fake{}},
		TrackingMethod:  TrackingMethodLabel,
	}
	gc, _ := NewGraphCache(config)

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
				"labels": map[string]interface{}{
					"app.kubernetes.io/instance": "test-app",
				},
			},
		},
	}

	// Simulate ADD event
	gc.handleResourceEvent(watch.Added, obj)
	key := ToResourceKey(obj)
	_, exists := gc.graph.Get(key)
	assert.True(t, exists)

	// Simulate DELETE event
	gc.handleResourceEvent(watch.Deleted, obj)
	_, exists = gc.graph.Get(key)
	assert.False(t, exists)
}

func TestGraphCache_Metrics(t *testing.T) {
	config := Config{
		DynamicClient:   fakedynamic.NewSimpleDynamicClient(runtime.NewScheme()),
		DiscoveryClient: &fakediscovery.FakeDiscovery{Fake: &kubetesting.Fake{}},
		TrackingMethod:  TrackingMethodLabel,
	}
	gc, _ := NewGraphCache(config)

	metrics := gc.GetMetrics()
	assert.Equal(t, 0, metrics.TotalManagedResources)

	// Add resource
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
				"labels": map[string]interface{}{
					"app.kubernetes.io/instance": "test-app",
				},
			},
		},
	}
	gc.handleResourceEvent(watch.Added, obj)

	metrics = gc.GetMetrics()
	assert.Equal(t, 1, metrics.TotalManagedResources)
	assert.Equal(t, int64(1), metrics.AddEvents)
	assert.Equal(t, 1, metrics.ResourcesByApplication["test-app"])
}
