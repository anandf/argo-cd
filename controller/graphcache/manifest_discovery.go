package graphcache

import (
	"context"
	"fmt"

	graphcore "github.com/argoproj/argo-cd/gitops-engine/pkg/graphcache"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	repoclient "github.com/argoproj/argo-cd/v3/reposerver/apiclient"
)

// ManifestDiscovery discovers resource types from application manifests
// and proactively creates watches before resources are synced.
type ManifestDiscovery struct {
	repoServerClient  repoclient.RepoServerServiceClient
	typeRelationships *graphcore.TypeRelationshipCache
	graphCache        *GraphCache
}

// ManifestDiscoveryConfig configures the manifest discovery component.
type ManifestDiscoveryConfig struct {
	RepoServerClient  repoclient.RepoServerServiceClient
	TypeRelationships *graphcore.TypeRelationshipCache
	GraphCache        *GraphCache
}

// NewManifestDiscovery creates a new manifest discovery component.
func NewManifestDiscovery(config ManifestDiscoveryConfig) *ManifestDiscovery {
	return &ManifestDiscovery{
		repoServerClient:  config.RepoServerClient,
		typeRelationships: config.TypeRelationships,
		graphCache:        config.GraphCache,
	}
}

// DiscoverFromApplication discovers all resource types from an application's manifests
// and creates watches for those types and their descendants.
func (m *ManifestDiscovery) DiscoverFromApplication(ctx context.Context, app *appv1.Application) error {
	appKey := fmt.Sprintf("%s/%s", app.Namespace, app.Name)
	log.WithField("app", appKey).Info("Starting manifest-based resource discovery")

	gvks, err := m.fetchAndParseManifests(ctx, app)
	if err != nil {
		return fmt.Errorf("failed to fetch manifests: %w", err)
	}

	return m.createWatches(app, gvks)
}

// fetchAndParseManifests fetches manifests from repo server and extracts GVKs.
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

	resp, err := m.repoServerClient.GenerateManifest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("repo server GenerateManifest failed: %w", err)
	}

	gvks := make(map[schema.GroupVersionKind]bool)

	for _, manifestStr := range resp.Manifests {
		if manifestStr == "" {
			continue
		}

		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(manifestStr), obj); err != nil {
			log.WithError(err).Warn("Failed to parse manifest, skipping")
			continue
		}

		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" {
			continue
		}

		gvks[gvk] = true

		m.extractReferencedResources(obj, gvks)
	}

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
// (e.g., ConfigMaps and Secrets referenced in Pod specs).
func (m *ManifestDiscovery) extractReferencedResources(obj *unstructured.Unstructured, gvks map[schema.GroupVersionKind]bool) {
	podSpec := m.extractPodSpec(obj)
	if podSpec == nil {
		return
	}

	configMapGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	secretGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}

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

	containers, found, err := unstructured.NestedSlice(podSpec, "containers")
	if found && err == nil {
		m.extractContainerRefs(containers, gvks, configMapGVK, secretGVK)
	}

	initContainers, found, err := unstructured.NestedSlice(podSpec, "initContainers")
	if found && err == nil {
		m.extractContainerRefs(initContainers, gvks, configMapGVK, secretGVK)
	}
}

// extractContainerRefs extracts ConfigMap/Secret references from a list of containers.
func (m *ManifestDiscovery) extractContainerRefs(containers []interface{}, gvks map[schema.GroupVersionKind]bool, configMapGVK, secretGVK schema.GroupVersionKind) {
	for _, cont := range containers {
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

// extractPodSpec extracts the pod template spec from a workload resource.
func (m *ManifestDiscovery) extractPodSpec(obj *unstructured.Unstructured) map[string]interface{} {
	gvk := obj.GroupVersionKind()

	if gvk.Kind == "Pod" {
		spec, found, err := unstructured.NestedMap(obj.Object, "spec")
		if found && err == nil {
			return spec
		}
	}

	podSpec, found, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec")
	if found && err == nil {
		return podSpec
	}

	return nil
}

// createWatches creates watches for the discovered GVKs and their descendants.
func (m *ManifestDiscovery) createWatches(app *appv1.Application, manifestGVKs []schema.GroupVersionKind) error {
	appKey := fmt.Sprintf("%s/%s", app.Namespace, app.Name)

	allGVKs := m.expandWithDescendants(manifestGVKs)

	log.WithField("app", appKey).
		WithField("manifestGVKs", len(manifestGVKs)).
		WithField("totalGVKs", len(allGVKs)).
		Info("Creating watches for discovered resource types")

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
		}
	}

	return nil
}

// expandWithDescendants expands a list of GVKs with their known descendants.
func (m *ManifestDiscovery) expandWithDescendants(gvks []schema.GroupVersionKind) []schema.GroupVersionKind {
	result := make(map[schema.GroupVersionKind]bool)

	for _, gvk := range gvks {
		result[gvk] = true

		descendants := m.typeRelationships.GetAllDescendantsRecursive(gvk)
		for _, desc := range descendants {
			result[desc] = true
		}
	}

	allGVKs := make([]schema.GroupVersionKind, 0, len(result))
	for gvk := range result {
		allGVKs = append(allGVKs, gvk)
	}

	return allGVKs
}
