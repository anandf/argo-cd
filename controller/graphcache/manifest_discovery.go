package graphcache

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	repoclient "github.com/argoproj/argo-cd/v3/reposerver/apiclient"
)

// ManifestDiscovery discovers resource types from application manifests
// and proactively creates watches before resources are synced
type ManifestDiscovery struct {
	repoServerClient  repoclient.RepoServerServiceClient
	typeRelationships *TypeRelationshipCache
	graphCache        *GraphCache

	// Cache manifest GVKs to avoid repeated repo server calls
	mu            sync.RWMutex
	manifestCache map[string]*manifestCacheEntry
	cacheTTL      time.Duration
}

// manifestCacheEntry caches discovered GVKs from an application's manifests
type manifestCacheEntry struct {
	gvks         []schema.GroupVersionKind
	commitSHA    string
	discoveredAt time.Time
}

// ManifestDiscoveryConfig configures the manifest discovery component
type ManifestDiscoveryConfig struct {
	RepoServerClient  repoclient.RepoServerServiceClient
	TypeRelationships *TypeRelationshipCache
	GraphCache        *GraphCache
	CacheTTL          time.Duration // How long to cache manifest discoveries
}

// NewManifestDiscovery creates a new manifest discovery component
func NewManifestDiscovery(config ManifestDiscoveryConfig) *ManifestDiscovery {
	if config.CacheTTL == 0 {
		config.CacheTTL = 5 * time.Minute // Default cache TTL
	}

	return &ManifestDiscovery{
		repoServerClient:  config.RepoServerClient,
		typeRelationships: config.TypeRelationships,
		graphCache:        config.GraphCache,
		manifestCache:     make(map[string]*manifestCacheEntry),
		cacheTTL:          config.CacheTTL,
	}
}

// DiscoverFromApplication discovers all resource types from an application's manifests
// and creates watches for those types and their descendants
func (m *ManifestDiscovery) DiscoverFromApplication(ctx context.Context, app *appv1.Application) error {
	appKey := fmt.Sprintf("%s/%s", app.Namespace, app.Name)
	log.WithField("app", appKey).Info("Starting manifest-based resource discovery")

	// Check cache first
	if gvks := m.getCachedGVKs(app); gvks != nil {
		log.WithField("app", appKey).Debug("Using cached manifest GVKs")
		return m.createWatches(app, gvks)
	}

	// Fetch manifests from repo server
	gvks, err := m.fetchAndParseManifests(ctx, app)
	if err != nil {
		return fmt.Errorf("failed to fetch manifests: %w", err)
	}

	// Cache the results
	m.cacheGVKs(app, gvks)

	// Create watches for discovered types and their descendants
	return m.createWatches(app, gvks)
}

// fetchAndParseManifests fetches manifests from repo server and extracts GVKs
func (m *ManifestDiscovery) fetchAndParseManifests(ctx context.Context, app *appv1.Application) ([]schema.GroupVersionKind, error) {
	source := app.Spec.Source
	if source == nil {
		if len(app.Spec.Sources) > 0 {
			source = &app.Spec.Sources[0]
		} else {
			return nil, fmt.Errorf("application %s/%s has no source configured", app.Namespace, app.Name)
		}
	}

	req := &repoclient.ManifestRequest{
		Repo: &appv1.Repository{
			Repo: source.RepoURL,
		},
		Revision:          source.TargetRevision,
		AppName:           app.Name,
		Namespace:         app.Spec.Destination.Namespace,
		ApplicationSource: source,
	}

	log.WithField("app", fmt.Sprintf("%s/%s", app.Namespace, app.Name)).
		WithField("repo", source.RepoURL).
		Debug("Fetching manifests from repo server")

	// Call repo server to generate manifests
	resp, err := m.repoServerClient.GenerateManifest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("repo server GenerateManifest failed: %w", err)
	}

	// Parse manifests and extract GVKs
	gvks := make(map[schema.GroupVersionKind]bool)

	for _, manifestStr := range resp.Manifests {
		if manifestStr == "" {
			continue
		}

		// Parse YAML
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(manifestStr), obj); err != nil {
			log.WithError(err).Warn("Failed to parse manifest, skipping")
			continue
		}

		// Extract GVK
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" {
			continue // Skip resources without a kind
		}

		gvks[gvk] = true

		// Also extract referenced resources (ConfigMaps, Secrets from Pod specs)
		m.extractReferencedResources(obj, gvks)
	}

	// Convert map to slice
	result := make([]schema.GroupVersionKind, 0, len(gvks))
	for gvk := range gvks {
		result = append(result, gvk)
	}

	log.WithField("app", fmt.Sprintf("%s/%s", app.Namespace, app.Name)).
		WithField("gvkCount", len(result)).
		Info("Discovered GVKs from manifests")

	return result, nil
}

// extractReferencedResources extracts GVKs of resources referenced by the manifest
// (e.g., ConfigMaps and Secrets referenced in Pod specs)
func (m *ManifestDiscovery) extractReferencedResources(obj *unstructured.Unstructured, gvks map[schema.GroupVersionKind]bool) {
	// Get Pod template spec if this is a workload resource
	podSpec := m.extractPodSpec(obj)
	if podSpec == nil {
		return
	}

	// ConfigMap and Secret are always v1
	configMapGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	secretGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}

	// Check volumes for ConfigMap and Secret references
	volumes, found, err := unstructured.NestedSlice(podSpec, "volumes")
	if found && err == nil {
		for _, vol := range volumes {
			volume, ok := vol.(map[string]interface{})
			if !ok {
				continue
			}

			if _, hasConfigMap := volume["configMap"]; hasConfigMap {
				gvks[configMapGVK] = true
			}
			if _, hasSecret := volume["secret"]; hasSecret {
				gvks[secretGVK] = true
			}
		}
	}

	// Check env variables for ConfigMap and Secret references
	containers, found, err := unstructured.NestedSlice(podSpec, "containers")
	if found && err == nil {
		for _, cont := range containers {
			container, ok := cont.(map[string]interface{})
			if !ok {
				continue
			}

			// Check envFrom
			envFrom, found, err := unstructured.NestedSlice(container, "envFrom")
			if found && err == nil {
				for _, ef := range envFrom {
					envFromSource, ok := ef.(map[string]interface{})
					if !ok {
						continue
					}
					if _, hasConfigMapRef := envFromSource["configMapRef"]; hasConfigMapRef {
						gvks[configMapGVK] = true
					}
					if _, hasSecretRef := envFromSource["secretRef"]; hasSecretRef {
						gvks[secretGVK] = true
					}
				}
			}

			// Check env (individual entries)
			env, found, err := unstructured.NestedSlice(container, "env")
			if found && err == nil {
				for _, e := range env {
					envVar, ok := e.(map[string]interface{})
					if !ok {
						continue
					}
					valueFrom, found, err := unstructured.NestedMap(envVar, "valueFrom")
					if !found || err != nil {
						continue
					}
					if _, hasConfigMapKeyRef := valueFrom["configMapKeyRef"]; hasConfigMapKeyRef {
						gvks[configMapGVK] = true
					}
					if _, hasSecretKeyRef := valueFrom["secretKeyRef"]; hasSecretKeyRef {
						gvks[secretGVK] = true
					}
				}
			}
		}
	}

	// Also check init containers
	initContainers, found, err := unstructured.NestedSlice(podSpec, "initContainers")
	if found && err == nil {
		// Same logic as regular containers - could be refactored into helper
		for _, cont := range initContainers {
			container, ok := cont.(map[string]interface{})
			if !ok {
				continue
			}

			envFrom, found, err := unstructured.NestedSlice(container, "envFrom")
			if found && err == nil {
				for _, ef := range envFrom {
					envFromSource, ok := ef.(map[string]interface{})
					if !ok {
						continue
					}
					if _, hasConfigMapRef := envFromSource["configMapRef"]; hasConfigMapRef {
						gvks[configMapGVK] = true
					}
					if _, hasSecretRef := envFromSource["secretRef"]; hasSecretRef {
						gvks[secretGVK] = true
					}
				}
			}
		}
	}
}

// extractPodSpec extracts the pod template spec from a workload resource
func (m *ManifestDiscovery) extractPodSpec(obj *unstructured.Unstructured) map[string]interface{} {
	gvk := obj.GroupVersionKind()

	// Direct pod
	if gvk.Kind == "Pod" {
		spec, found, err := unstructured.NestedMap(obj.Object, "spec")
		if found && err == nil {
			return spec
		}
	}

	// Workload resources with pod template
	podSpec, found, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec")
	if found && err == nil {
		return podSpec
	}

	return nil
}

// createWatches creates watches for the discovered GVKs and their descendants
func (m *ManifestDiscovery) createWatches(app *appv1.Application, manifestGVKs []schema.GroupVersionKind) error {
	appKey := fmt.Sprintf("%s/%s", app.Namespace, app.Name)

	// Expand with known descendants
	allGVKs := m.expandWithDescendants(manifestGVKs)

	log.WithField("app", appKey).
		WithField("manifestGVKs", len(manifestGVKs)).
		WithField("totalGVKs", len(allGVKs)).
		Info("Creating watches for discovered resource types")

	// Create watches for all types
	for _, gvk := range allGVKs {
		namespace := app.Spec.Destination.Namespace
		if namespace == "" {
			namespace = metav1.NamespaceDefault
		}

		if err := m.graphCache.EnsureWatch(gvk, namespace); err != nil {
			log.WithError(err).
				WithField("gvk", gvk.String()).
				WithField("namespace", namespace).
				Warn("Failed to create watch, will retry later")
			// Continue with other watches rather than failing completely
		}
	}

	return nil
}

// expandWithDescendants expands a list of GVKs with their known descendants
func (m *ManifestDiscovery) expandWithDescendants(gvks []schema.GroupVersionKind) []schema.GroupVersionKind {
	result := make(map[schema.GroupVersionKind]bool)

	for _, gvk := range gvks {
		// Add the original GVK
		result[gvk] = true

		// Add all known descendants recursively
		descendants := m.typeRelationships.GetAllDescendantsRecursive(gvk)
		for _, desc := range descendants {
			result[desc] = true
		}
	}

	// Convert map to slice
	allGVKs := make([]schema.GroupVersionKind, 0, len(result))
	for gvk := range result {
		allGVKs = append(allGVKs, gvk)
	}

	return allGVKs
}

// getCachedGVKs retrieves cached GVKs if they're still valid
func (m *ManifestDiscovery) getCachedGVKs(app *appv1.Application) []schema.GroupVersionKind {
	m.mu.RLock()
	defer m.mu.RUnlock()

	appKey := fmt.Sprintf("%s/%s", app.Namespace, app.Name)
	entry, exists := m.manifestCache[appKey]
	if !exists {
		return nil
	}

	// Check if cache is still valid
	if time.Since(entry.discoveredAt) > m.cacheTTL {
		return nil // Cache expired
	}

	// Check if commit SHA changed (if available)
	currentSHA := m.getCommitSHA(app)
	if currentSHA != "" && entry.commitSHA != "" && entry.commitSHA != currentSHA {
		return nil // Source changed
	}

	return entry.gvks
}

// cacheGVKs stores discovered GVKs in the cache
func (m *ManifestDiscovery) cacheGVKs(app *appv1.Application, gvks []schema.GroupVersionKind) {
	m.mu.Lock()
	defer m.mu.Unlock()

	appKey := fmt.Sprintf("%s/%s", app.Namespace, app.Name)
	m.manifestCache[appKey] = &manifestCacheEntry{
		gvks:         gvks,
		commitSHA:    m.getCommitSHA(app),
		discoveredAt: time.Now(),
	}
}

// getCommitSHA extracts the commit SHA from application status if available
func (m *ManifestDiscovery) getCommitSHA(app *appv1.Application) string {
	if app.Status.Sync.Revision != "" {
		return app.Status.Sync.Revision
	}
	// Could also check app.Status.OperationState.SyncResult.Revision
	return ""
}

// InvalidateCache invalidates the manifest cache for an application
func (m *ManifestDiscovery) InvalidateCache(appNamespace, appName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	appKey := fmt.Sprintf("%s/%s", appNamespace, appName)
	delete(m.manifestCache, appKey)
}

// ClearExpiredCache removes expired entries from the cache
func (m *ManifestDiscovery) ClearExpiredCache() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for key, entry := range m.manifestCache {
		if now.Sub(entry.discoveredAt) > m.cacheTTL {
			delete(m.manifestCache, key)
		}
	}
}

// GetCacheStats returns statistics about the manifest cache
func (m *ManifestDiscovery) GetCacheStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := map[string]interface{}{
		"total_entries": len(m.manifestCache),
		"cache_ttl":     m.cacheTTL.String(),
	}

	// Count expired entries
	expired := 0
	now := time.Now()
	for _, entry := range m.manifestCache {
		if now.Sub(entry.discoveredAt) > m.cacheTTL {
			expired++
		}
	}
	stats["expired_entries"] = expired

	return stats
}


