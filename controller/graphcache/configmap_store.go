package graphcache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ConfigMapStore implements RelationshipStore using Kubernetes ConfigMaps
type ConfigMapStore struct {
	kubeClient    kubernetes.Interface
	namespace     string
	configMapName string

	mu            sync.Mutex
	lastSaved     time.Time
	saveCount     int64
}

// ConfigMapStoreConfig configures the ConfigMap store
type ConfigMapStoreConfig struct {
	KubeClient    kubernetes.Interface
	Namespace     string
	ConfigMapName string // Optional, defaults to RelationshipConfigMapName
}

// NewConfigMapStore creates a new ConfigMap-based relationship store
func NewConfigMapStore(config ConfigMapStoreConfig) *ConfigMapStore {
	if config.ConfigMapName == "" {
		config.ConfigMapName = RelationshipConfigMapName
	}

	return &ConfigMapStore{
		kubeClient:    config.KubeClient,
		namespace:     config.Namespace,
		configMapName: config.ConfigMapName,
	}
}

// Save persists relationships to a ConfigMap
func (s *ConfigMapStore) Save(relationships []PersistedRelationship, metadata PersistedRelationshipMetadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create persisted structure
	persisted := PersistedRelationships{
		Version:       "v1",
		LastUpdated:   time.Now(),
		Relationships: relationships,
		Metadata:      metadata,
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal relationships: %w", err)
	}

	// Check ConfigMap size (1MB limit)
	if len(data) > 1*1024*1024 {
		log.WithFields(log.Fields{
			"component": "graph-cache",
			"store":     "configmap",
			"size":      len(data),
			"limit":     1*1024*1024,
		}).Warn("Relationship data exceeds ConfigMap size limit")
		// TODO: Implement truncation strategy (keep highest confidence)
		return fmt.Errorf("data size %d exceeds ConfigMap 1MB limit", len(data))
	}

	// Create ConfigMap object
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.configMapName,
			Namespace: s.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "argocd-graph-cache",
				"app.kubernetes.io/component": "relationship-cache",
				"app.kubernetes.io/part-of":   "argocd",
			},
		},
		Data: map[string]string{
			RelationshipDataKey: string(data),
		},
	}

	// Try to update existing ConfigMap
	existingCM, err := s.kubeClient.CoreV1().ConfigMaps(s.namespace).Get(
		context.Background(),
		s.configMapName,
		metav1.GetOptions{},
	)

	if err == nil {
		// ConfigMap exists, update it
		cm.ResourceVersion = existingCM.ResourceVersion
		_, err = s.kubeClient.CoreV1().ConfigMaps(s.namespace).Update(
			context.Background(),
			cm,
			metav1.UpdateOptions{},
		)
	} else if apierrors.IsNotFound(err) {
		// ConfigMap doesn't exist, create it
		_, err = s.kubeClient.CoreV1().ConfigMaps(s.namespace).Create(
			context.Background(),
			cm,
			metav1.CreateOptions{},
		)
	}

	if err != nil {
		return fmt.Errorf("failed to save configmap: %w", err)
	}

	s.lastSaved = time.Now()
	s.saveCount++

	log.WithFields(log.Fields{
		"component":    "graph-cache",
		"store":        "configmap",
		"namespace":    s.namespace,
		"configmap":    s.configMapName,
		"size_bytes":   len(data),
		"save_count":   s.saveCount,
	}).Debug("Saved relationships to ConfigMap")

	return nil
}

// Load retrieves relationships from the ConfigMap
func (s *ConfigMapStore) Load() ([]PersistedRelationship, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cm, err := s.kubeClient.CoreV1().ConfigMaps(s.namespace).Get(
		context.Background(),
		s.configMapName,
		metav1.GetOptions{},
	)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithFields(log.Fields{
				"component": "graph-cache",
				"store":     "configmap",
			}).Info("No persisted relationships found")
			return []PersistedRelationship{}, nil
		}
		return nil, fmt.Errorf("failed to get configmap: %w", err)
	}

	data, ok := cm.Data[RelationshipDataKey]
	if !ok {
		log.WithFields(log.Fields{
			"component": "graph-cache",
			"store":     "configmap",
		}).Warn("ConfigMap exists but has no relationship data")
		return []PersistedRelationship{}, nil
	}

	var persisted PersistedRelationships
	if err := json.Unmarshal([]byte(data), &persisted); err != nil {
		return nil, fmt.Errorf("failed to unmarshal relationships: %w", err)
	}

	log.WithFields(log.Fields{
		"component":    "graph-cache",
		"store":        "configmap",
		"loaded":       len(persisted.Relationships),
		"last_updated": persisted.LastUpdated,
		"seeded":       persisted.Metadata.SeededCount,
		"learned":      persisted.Metadata.LearnedCount,
	}).Info("Loaded relationships from ConfigMap")

	return persisted.Relationships, nil
}

// Close performs cleanup (no-op for ConfigMap store)
func (s *ConfigMapStore) Close() error {
	// ConfigMap store doesn't need cleanup
	return nil
}

// GetStats returns statistics about the ConfigMap store
func (s *ConfigMapStore) GetStats() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := map[string]interface{}{
		"type":          "configmap",
		"namespace":     s.namespace,
		"configmap":     s.configMapName,
		"last_saved":    s.lastSaved,
		"save_count":    s.saveCount,
	}

	if !s.lastSaved.IsZero() {
		stats["time_since_last_save"] = time.Since(s.lastSaved).String()
	}

	return stats
}
