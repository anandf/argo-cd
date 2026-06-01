package graphcache

import (
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// CustomRelationshipsFile represents the YAML structure for custom relationship definitions.
type CustomRelationshipsFile struct {
	Relationships []CustomRelationshipEntry `json:"relationships"`
}

// CustomRelationshipEntry defines a single parent→child type relationship.
type CustomRelationshipEntry struct {
	Parent GVKRef `json:"parent"`
	Child  GVKRef `json:"child"`
}

// GVKRef is a YAML-friendly GroupVersionKind reference.
type GVKRef struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

func (r GVKRef) toGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: r.Group, Version: r.Version, Kind: r.Kind}
}

// LoadCustomRelationships reads a YAML file defining custom parent→child type relationships.
// Returns an empty slice and nil error if the file does not exist.
// Returns an error if the file exists but cannot be parsed or contains invalid entries.
func LoadCustomRelationships(log logr.Logger, filePath string) ([]TypeRelationship, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read custom relationships file: %w", err)
	}

	var file CustomRelationshipsFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("failed to parse custom relationships file: %w", err)
	}

	var result []TypeRelationship
	for i, entry := range file.Relationships {
		if entry.Parent.Kind == "" || entry.Parent.Version == "" {
			log.Info("Skipping custom relationship with missing parent kind or version", "index", i)
			continue
		}
		if entry.Child.Kind == "" || entry.Child.Version == "" {
			log.Info("Skipping custom relationship with missing child kind or version", "index", i)
			continue
		}
		result = append(result, TypeRelationship{
			Parent: entry.Parent.toGVK(),
			Child:  entry.Child.toGVK(),
		})
	}

	return result, nil
}
