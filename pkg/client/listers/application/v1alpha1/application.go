// Code generated by lister-gen. DO NOT EDIT.

package v1alpha1

import (
	v1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/listers"
	"k8s.io/client-go/tools/cache"
)

// ApplicationLister helps list Applications.
// All objects returned here must be treated as read-only.
type ApplicationLister interface {
	// List lists all Applications in the indexer.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1alpha1.Application, err error)
	// Applications returns an object that can list and get Applications.
	Applications(namespace string) ApplicationNamespaceLister
	ApplicationListerExpansion
}

// applicationLister implements the ApplicationLister interface.
type applicationLister struct {
	listers.ResourceIndexer[*v1alpha1.Application]
}

// NewApplicationLister returns a new ApplicationLister.
func NewApplicationLister(indexer cache.Indexer) ApplicationLister {
	return &applicationLister{listers.New[*v1alpha1.Application](indexer, v1alpha1.Resource("application"))}
}

// Applications returns an object that can list and get Applications.
func (s *applicationLister) Applications(namespace string) ApplicationNamespaceLister {
	return applicationNamespaceLister{listers.NewNamespaced[*v1alpha1.Application](s.ResourceIndexer, namespace)}
}

// ApplicationNamespaceLister helps list and get Applications.
// All objects returned here must be treated as read-only.
type ApplicationNamespaceLister interface {
	// List lists all Applications in the indexer for a given namespace.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1alpha1.Application, err error)
	// Get retrieves the Application from the indexer for a given namespace and name.
	// Objects returned here must be treated as read-only.
	Get(name string) (*v1alpha1.Application, error)
	ApplicationNamespaceListerExpansion
}

// applicationNamespaceLister implements the ApplicationNamespaceLister
// interface.
type applicationNamespaceLister struct {
	listers.ResourceIndexer[*v1alpha1.Application]
}
