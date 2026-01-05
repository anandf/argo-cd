package graphcache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"
)

type mockDiscovery struct {
	fakediscovery.FakeDiscovery
	resources []*metav1.APIResourceList
}

func (m *mockDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return m.resources, nil
}

func TestNewSelectiveWatchManager(t *testing.T) {
	scheme := runtime.NewScheme()
	dynamicClient := fakedynamic.NewSimpleDynamicClient(scheme)
	discoveryClient := &fakediscovery.FakeDiscovery{
		Fake: &kubetesting.Fake{},
	}

	wm := NewSelectiveWatchManager(
		dynamicClient,
		discoveryClient,
		TrackingMethodLabel,
		[]string{"argocd"},
		nil,
	)

	assert.NotNil(t, wm)
	assert.Equal(t, TrackingMethodLabel, wm.trackingMethod)
	assert.Equal(t, []string{"argocd"}, wm.namespaces)
}

func TestEnsureWatch_ClusterScoped(t *testing.T) {
	scheme := runtime.NewScheme()
	dynamicClient := fakedynamic.NewSimpleDynamicClient(scheme)
	
	resources := []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "nodes",
					Kind:       "Node",
					Namespaced: false,
				},
			},
		},
	}
	
	discoveryClient := &mockDiscovery{
		FakeDiscovery: fakediscovery.FakeDiscovery{
			Fake: &kubetesting.Fake{},
		},
		resources: resources,
	}

	wm := NewSelectiveWatchManager(
		dynamicClient,
		discoveryClient,
		TrackingMethodLabel,
		[]string{"argocd"}, // Should be ignored for cluster scoped
		nil,
	)

	gk := schema.GroupKind{Group: "", Kind: "Node"}
	created, err := wm.EnsureWatch(gk, "")

	assert.NoError(t, err)
	assert.True(t, created)
	
	// Wait for goroutine
	time.Sleep(100 * time.Millisecond)

	// Verify Watch action
	actions := dynamicClient.Actions()
	watchActionFound := false
	for _, action := range actions {
		if action.GetVerb() == "watch" && action.GetResource().Resource == "nodes" {
			watchActionFound = true
			break
		}
	}
	assert.True(t, watchActionFound, "Should have watched nodes")
}

func TestEnsureWatch_DynamicNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	dynamicClient := fakedynamic.NewSimpleDynamicClient(scheme)
	
	resources := []*metav1.APIResourceList{
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "deployments",
					Kind:       "Deployment",
					Namespaced: true,
				},
			},
		},
	}

	discoveryClient := &mockDiscovery{
		FakeDiscovery: fakediscovery.FakeDiscovery{
			Fake: &kubetesting.Fake{},
		},
		resources: resources,
	}

	namespaces := []string{"ns1"}
	wm := NewSelectiveWatchManager(
		dynamicClient,
		discoveryClient,
		TrackingMethodLabel,
		namespaces,
		nil,
	)

	gk := schema.GroupKind{Group: "apps", Kind: "Deployment"}
	
	// 1. Initial Watch (ns1)
	created, err := wm.EnsureWatch(gk, "ns1")
	assert.NoError(t, err)
	assert.True(t, created)
	
	// Wait a bit
	time.Sleep(50 * time.Millisecond)
	
	// 2. Add new namespace (ns2)
	created, err = wm.EnsureWatch(gk, "ns2")
	assert.NoError(t, err)
	assert.True(t, created, "Should extend watch to ns2")
	
	// Wait a bit
	time.Sleep(50 * time.Millisecond)

	// Verify Watch actions for BOTH namespaces
	actions := dynamicClient.Actions()
	ns1Watched := false
	ns2Watched := false

	for _, action := range actions {
		if action.GetVerb() == "watch" && action.GetResource().Resource == "deployments" {
			if action.GetNamespace() == "ns1" {
				ns1Watched = true
			} else if action.GetNamespace() == "ns2" {
				ns2Watched = true
			}
		}
	}

	assert.True(t, ns1Watched, "Should have watched ns1")
	assert.True(t, ns2Watched, "Should have watched ns2")
}
