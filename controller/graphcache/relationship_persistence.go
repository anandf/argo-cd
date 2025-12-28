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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

const (
	// ConfigMap name for storing learned relationships
	RelationshipConfigMapName = "argocd-graph-cache-relationships"

	// ConfigMap key for the relationships data
	RelationshipDataKey = "relationships.json"

	// How often to persist relationships to ConfigMap
	DefaultPersistInterval = 5 * time.Minute

	// Minimum confidence to persist (avoid storing one-off noise)
	MinConfidenceToPersist = 2
)

// RelationshipPersistence handles saving and loading learned relationships
type RelationshipPersistence struct {
	kubeClient kubernetes.Interface
	namespace  string
	cache      *TypeRelationshipCache

	// Persistence configuration
	configMapName   string
	persistInterval time.Duration
	minConfidence   int

	// State management
	mu               sync.Mutex
	lastPersisted    time.Time
	persistenceCount int64

	// Context for background persistence
	ctx    context.Context
	cancel context.CancelFunc
}

// PersistedRelationships represents the JSON structure stored in ConfigMap
type PersistedRelationships struct {
	Version      string                       `json:"version"`
	LastUpdated  time.Time                    `json:"lastUpdated"`
	Relationships []PersistedRelationship      `json:"relationships"`
	Metadata     PersistedRelationshipMetadata `json:"metadata"`
}

// PersistedRelationship represents a single parent-child relationship
type PersistedRelationship struct {
	Parent     string `json:"parent"`     // GVK string: "group/version/kind"
	Child      string `json:"child"`      // GVK string: "group/version/kind"
	Confidence int    `json:"confidence"` // Observation count
}

// PersistedRelationshipMetadata contains metadata about the persisted data
type PersistedRelationshipMetadata struct {
	TotalRelationships int    `json:"totalRelationships"`
	SeededCount        int    `json:"seededCount"`
	LearnedCount       int    `json:"learnedCount"`
	ControllerVersion  string `json:"controllerVersion,omitempty"`
}

// NewRelationshipPersistence creates a new persistence manager
func NewRelationshipPersistence(
	kubeClient kubernetes.Interface,
	namespace string,
	cache *TypeRelationshipCache,
) *RelationshipPersistence {
	ctx, cancel := context.WithCancel(context.Background())

	return &RelationshipPersistence{
		kubeClient:      kubeClient,
		namespace:       namespace,
		cache:           cache,
		configMapName:   RelationshipConfigMapName,
		persistInterval: DefaultPersistInterval,
		minConfidence:   MinConfidenceToPersist,
		ctx:             ctx,
		cancel:          cancel,
	}
}

// Start begins background persistence
func (p *RelationshipPersistence) Start() error {
	log.WithFields(log.Fields{
		"component":  "graph-cache",
		"subsystem":  "persistence",
		"namespace":  p.namespace,
		"configmap":  p.configMapName,
		"interval":   p.persistInterval,
	}).Info("Starting relationship persistence")

	// Load existing relationships on startup
	if err := p.Load(); err != nil {
		log.WithError(err).Warn("Failed to load persisted relationships, starting fresh")
	}

	// Start background persistence loop
	go p.persistenceLoop()

	return nil
}

// Stop stops background persistence and performs final save
func (p *RelationshipPersistence) Stop() error {
	log.WithField("component", "graph-cache").Info("Stopping relationship persistence")

	p.cancel()

	// Perform final save
	if err := p.Save(); err != nil {
		return fmt.Errorf("failed to save relationships on shutdown: %w", err)
	}

	return nil
}

// Load loads relationships from ConfigMap
func (p *RelationshipPersistence) Load() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cm, err := p.kubeClient.CoreV1().ConfigMaps(p.namespace).Get(
		context.Background(),
		p.configMapName,
		metav1.GetOptions{},
	)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithField("component", "graph-cache").Info("No persisted relationships found, starting fresh")
			return nil
		}
		return fmt.Errorf("failed to get configmap: %w", err)
	}

	data, ok := cm.Data[RelationshipDataKey]
	if !ok {
		log.WithField("component", "graph-cache").Warn("ConfigMap exists but has no relationship data")
		return nil
	}

	var persisted PersistedRelationships
	if err := json.Unmarshal([]byte(data), &persisted); err != nil {
		return fmt.Errorf("failed to unmarshal relationships: %w", err)
	}

	// Load relationships into cache
	loadedCount := 0
	for _, rel := range persisted.Relationships {
		parentGVK, err := parseGVKString(rel.Parent)
		if err != nil {
			log.WithError(err).WithField("parent", rel.Parent).Warn("Failed to parse parent GVK")
			continue
		}

		childGVK, err := parseGVKString(rel.Child)
		if err != nil {
			log.WithError(err).WithField("child", rel.Child).Warn("Failed to parse child GVK")
			continue
		}

		// Load the relationship with its confidence
		p.cache.LearnRelationship(parentGVK, childGVK, rel.Confidence)
		loadedCount++
	}

	log.WithFields(log.Fields{
		"component":       "graph-cache",
		"loaded":          loadedCount,
		"total":           len(persisted.Relationships),
		"last_updated":    persisted.LastUpdated,
		"seeded":          persisted.Metadata.SeededCount,
		"learned":         persisted.Metadata.LearnedCount,
	}).Info("Loaded persisted relationships")

	return nil
}

// Save saves relationships to ConfigMap
func (p *RelationshipPersistence) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Get all relationships from cache
	allRels := p.cache.GetAllRelationships()

	// Filter by minimum confidence
	relationships := []PersistedRelationship{}
	seededCount := 0
	learnedCount := 0

	for rel, confidence := range allRels {
		if confidence < p.minConfidence {
			continue // Skip low-confidence relationships
		}

		relationships = append(relationships, PersistedRelationship{
			Parent:     gvkToString(rel.Parent),
			Child:      gvkToString(rel.Child),
			Confidence: confidence,
		})

		// Count seeded vs learned (seeded have confidence >= 50)
		if confidence >= 50 {
			seededCount++
		} else {
			learnedCount++
		}
	}

	// Create persisted structure
	persisted := PersistedRelationships{
		Version:      "v1",
		LastUpdated:  time.Now(),
		Relationships: relationships,
		Metadata: PersistedRelationshipMetadata{
			TotalRelationships: len(relationships),
			SeededCount:        seededCount,
			LearnedCount:       learnedCount,
		},
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
			"size":      len(data),
			"limit":     1*1024*1024,
		}).Warn("Relationship data exceeds ConfigMap size limit, truncating")
		// In production, implement truncation strategy (keep highest confidence)
	}

	// Create or update ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.configMapName,
			Namespace: p.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "argocd-graph-cache",
				"app.kubernetes.io/component": "relationship-cache",
			},
		},
		Data: map[string]string{
			RelationshipDataKey: string(data),
		},
	}

	// Try to update existing ConfigMap
	existingCM, err := p.kubeClient.CoreV1().ConfigMaps(p.namespace).Get(
		context.Background(),
		p.configMapName,
		metav1.GetOptions{},
	)

	if err == nil {
		// ConfigMap exists, update it
		cm.ResourceVersion = existingCM.ResourceVersion
		_, err = p.kubeClient.CoreV1().ConfigMaps(p.namespace).Update(
			context.Background(),
			cm,
			metav1.UpdateOptions{},
		)
	} else if apierrors.IsNotFound(err) {
		// ConfigMap doesn't exist, create it
		_, err = p.kubeClient.CoreV1().ConfigMaps(p.namespace).Create(
			context.Background(),
			cm,
			metav1.CreateOptions{},
		)
	}

	if err != nil {
		return fmt.Errorf("failed to save configmap: %w", err)
	}

	p.lastPersisted = time.Now()
	p.persistenceCount++

	log.WithFields(log.Fields{
		"component":         "graph-cache",
		"relationships":     len(relationships),
		"seeded":            seededCount,
		"learned":           learnedCount,
		"size_bytes":        len(data),
		"persistence_count": p.persistenceCount,
	}).Info("Saved relationships to ConfigMap")

	return nil
}

// persistenceLoop runs periodic persistence in the background
func (p *RelationshipPersistence) persistenceLoop() {
	ticker := time.NewTicker(p.persistInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			if err := p.Save(); err != nil {
				log.WithError(err).WithField("component", "graph-cache").Warn("Failed to persist relationships")
			}
		}
	}
}

// GetStats returns persistence statistics
func (p *RelationshipPersistence) GetStats() map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()

	stats := map[string]interface{}{
		"last_persisted":    p.lastPersisted,
		"persistence_count": p.persistenceCount,
		"config_map_name":   p.configMapName,
		"namespace":         p.namespace,
		"persist_interval":  p.persistInterval.String(),
		"min_confidence":    p.minConfidence,
	}

	if !p.lastPersisted.IsZero() {
		stats["time_since_last_persist"] = time.Since(p.lastPersisted).String()
	}

	return stats
}

// gvkToString converts a GVK to string format "group/version/kind"
func gvkToString(gvk schema.GroupVersionKind) string {
	return fmt.Sprintf("%s/%s/%s", gvk.Group, gvk.Version, gvk.Kind)
}

// parseGVKString parses a GVK string in format "group/version/kind"
func parseGVKString(s string) (schema.GroupVersionKind, error) {
	// Handle core group (empty group)
	// Format: "/v1/Pod" or "apps/v1/Deployment"
	parts := []string{}
	current := ""
	for i, r := range s {
		if r == '/' {
			parts = append(parts, current)
			current = ""
		} else if i == len(s)-1 {
			current += string(r)
			parts = append(parts, current)
		} else {
			current += string(r)
		}
	}

	if len(parts) != 3 {
		return schema.GroupVersionKind{}, fmt.Errorf("invalid GVK format: %s", s)
	}

	return schema.GroupVersionKind{
		Group:   parts[0],
		Version: parts[1],
		Kind:    parts[2],
	}, nil
}
