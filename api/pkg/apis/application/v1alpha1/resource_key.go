package v1alpha1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ResourceKey uniquely identifies a Kubernetes resource. The API version is deliberately
// excluded, so that the same resource served under different API versions maps to a single key.
//
// This mirrors github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube.ResourceKey. It is
// declared here, rather than imported, so that consumers of the Argo CD API types do not inherit
// gitops-engine — and through it k8s.io/kubernetes — in their dependency graph. The two are
// distinct Go types with identical layout, so converting between them is a plain struct
// conversion.
//
// It is a lookup key rather than API surface: it is never serialized, appears in no CRD field,
// and exists only as an argument to ApplicationStatus.FindResource. The markers below keep the
// generators from treating it as a CRD message — without them, go-to-protobuf emits a competing
// String() method into generated.pb.go.
//
// +k8s:deepcopy-gen=false
// +k8s:openapi-gen=false
// +protobuf=false
type ResourceKey struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

func (k *ResourceKey) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", k.Group, k.Kind, k.Namespace, k.Name)
}

func (k ResourceKey) GroupKind() schema.GroupKind {
	return schema.GroupKind{Group: k.Group, Kind: k.Kind}
}

func NewResourceKey(group string, kind string, namespace string, name string) ResourceKey {
	return ResourceKey{Group: group, Kind: kind, Namespace: namespace, Name: name}
}
