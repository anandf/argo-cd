package graphcache

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
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
	log           logr.Logger

	mu        sync.Mutex
	lastSaved time.Time
	saveCount int64
}

// ConfigMapStoreConfig configures the ConfigMap store
type ConfigMapStoreConfig struct {
	KubeClient    kubernetes.Interface
	Namespace     string
	ConfigMapName string // Optional, defaults to RelationshipConfigMapName
	Log           logr.Logger
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
		log:           config.Log,
	}
}

// Save persists relationships to a ConfigMap
func (s *ConfigMapStore) Save(relationships []PersistedRelationship, metadata PersistedRelationshipMetadata) error {
	persisted := PersistedRelationships{
		Version:       "v1",
		LastUpdated:   time.Now(),
		Relationships: relationships,
		Metadata:      metadata,
	}

	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal relationships: %w", err)
	}

	// Check ConfigMap size (1MB limit) and truncate if needed
	const maxConfigMapSize = 1 * 1024 * 1024
	if len(data) > maxConfigMapSize {
		s.log.Info("Relationship data exceeds ConfigMap size limit, truncating",
			"size", len(data), "limit", maxConfigMapSize, "relationships", len(relationships))

		// Sort alphabetically by parent+child for deterministic truncation
		sort.Slice(relationships, func(i, j int) bool {
			pi := relationships[i].Parent + "|" + relationships[i].Child
			pj := relationships[j].Parent + "|" + relationships[j].Child
			return pi < pj
		})

		// Binary search to find max count that fits
		totalCount := len(relationships)
		truncatedCount := sort.Search(totalCount, func(n int) bool {
			count := totalCount - n
			if count <= 0 {
				return true
			}
			truncatedPersisted := PersistedRelationships{
				Version:       "v1",
				LastUpdated:   time.Now(),
				Relationships: relationships[:count],
				Metadata: PersistedRelationshipMetadata{
					TotalRelationships: count,
					SeededCount:        metadata.SeededCount,
					LearnedCount:       metadata.LearnedCount,
				},
			}
			d, e := json.MarshalIndent(truncatedPersisted, "", "  ")
			if e != nil {
				return false
			}
			return len(d) <= maxConfigMapSize
		})
		keepCount := totalCount - truncatedCount
		if keepCount <= 0 {
			return fmt.Errorf("unable to truncate relationships to fit ConfigMap size limit")
		}

		truncatedPersisted := PersistedRelationships{
			Version:       "v1",
			LastUpdated:   time.Now(),
			Relationships: relationships[:keepCount],
			Metadata: PersistedRelationshipMetadata{
				TotalRelationships: keepCount,
				SeededCount:        metadata.SeededCount,
				LearnedCount:       metadata.LearnedCount,
			},
		}
		data, err = json.MarshalIndent(truncatedPersisted, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal truncated relationships: %w", err)
		}

		s.log.Info("Truncated relationships to fit ConfigMap size limit",
			"original_count", totalCount, "truncated_count", keepCount, "final_size", len(data))
	}

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

	ctx := context.TODO()
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		existingCM, getErr := s.kubeClient.CoreV1().ConfigMaps(s.namespace).Get(
			ctx,
			s.configMapName,
			metav1.GetOptions{},
		)

		if getErr == nil {
			cm.ResourceVersion = existingCM.ResourceVersion
			_, err = s.kubeClient.CoreV1().ConfigMaps(s.namespace).Update(
				ctx,
				cm,
				metav1.UpdateOptions{},
			)
			if err != nil && apierrors.IsConflict(err) && attempt < maxRetries-1 {
				continue
			}
		} else if apierrors.IsNotFound(getErr) {
			_, err = s.kubeClient.CoreV1().ConfigMaps(s.namespace).Create(
				ctx,
				cm,
				metav1.CreateOptions{},
			)
		} else {
			err = getErr
		}
		break
	}

	if err != nil {
		return fmt.Errorf("failed to save configmap: %w", err)
	}

	s.mu.Lock()
	s.lastSaved = time.Now()
	s.saveCount++
	s.mu.Unlock()

	s.log.V(1).Info("Saved relationships to ConfigMap",
		"namespace", s.namespace, "configmap", s.configMapName, "size_bytes", len(data))

	return nil
}

// Load retrieves relationships from the ConfigMap
func (s *ConfigMapStore) Load() ([]PersistedRelationship, error) {
	cm, err := s.kubeClient.CoreV1().ConfigMaps(s.namespace).Get(
		context.TODO(),
		s.configMapName,
		metav1.GetOptions{},
	)
	if err != nil {
		if apierrors.IsNotFound(err) {
			s.log.Info("No persisted relationships found")
			return []PersistedRelationship{}, nil
		}
		return nil, fmt.Errorf("failed to get configmap: %w", err)
	}

	data, ok := cm.Data[RelationshipDataKey]
	if !ok {
		s.log.Info("ConfigMap exists but has no relationship data")
		return []PersistedRelationship{}, nil
	}

	var persisted PersistedRelationships
	if err := json.Unmarshal([]byte(data), &persisted); err != nil {
		return nil, fmt.Errorf("failed to unmarshal relationships: %w", err)
	}

	s.log.Info("Loaded relationships from ConfigMap",
		"loaded", len(persisted.Relationships), "last_updated", persisted.LastUpdated)

	return persisted.Relationships, nil
}

// Close performs cleanup (no-op for ConfigMap store)
func (s *ConfigMapStore) Close() error {
	return nil
}
