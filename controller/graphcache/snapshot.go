package graphcache

import (
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
)

// GraphSnapshot represents a point-in-time snapshot of the resource graph
type GraphSnapshot struct {
	Version     string         `json:"version"`
	LastUpdated time.Time      `json:"lastUpdated"`
	Nodes       []SnapshotNode `json:"nodes"`
}

// SnapshotNode is a serializable representation of a ResourceNode
type SnapshotNode struct {
	Key             kube.ResourceKey   `json:"key"`
	Version         string             `json:"version"`
	UID             string             `json:"uid"`
	ResourceVersion string             `json:"resourceVersion"`
	ManagedBy       string             `json:"managedBy"`
	TrackingID      string             `json:"trackingID"`
	Parents         []ParentRef        `json:"parents"`
	Children        []kube.ResourceKey `json:"children,omitempty"`
	Info            *ResourceMetadata  `json:"info,omitempty"`
	CreatedAt       time.Time          `json:"createdAt"`
}

// GraphStore defines the interface for persisting graph snapshots
type GraphStore interface {
	// SaveSnapshot persists the graph snapshot for a specific cluster
	SaveSnapshot(clusterServer string, snapshot *GraphSnapshot) error

	// LoadSnapshot retrieves the graph snapshot for a specific cluster
	LoadSnapshot(clusterServer string) (*GraphSnapshot, error)
}
