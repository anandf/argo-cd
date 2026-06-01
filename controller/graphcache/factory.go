package graphcache

import (
	"fmt"

	graphcore "github.com/argoproj/argo-cd/gitops-engine/pkg/graphcache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/health"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2/textlogger"

	statecache "github.com/argoproj/argo-cd/v3/controller/cache"
	"github.com/argoproj/argo-cd/v3/controller/metrics"
	"github.com/argoproj/argo-cd/v3/controller/sharding"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/util/argo"
	"github.com/argoproj/argo-cd/v3/util/argo/normalizers"
	"github.com/argoproj/argo-cd/v3/util/db"
	"github.com/argoproj/argo-cd/v3/util/env"
	"github.com/argoproj/argo-cd/v3/util/lua"
	"github.com/argoproj/argo-cd/v3/util/settings"
)

const (
	// EnvGraphCacheEnabled enables the graph-based cache
	EnvGraphCacheEnabled = "ARGOCD_ENABLE_GRAPH_CACHE"

	// EnvGraphCacheTrackingMethod sets the tracking method for graph cache
	EnvGraphCacheTrackingMethod = "ARGOCD_GRAPH_CACHE_TRACKING_METHOD"

	// EnvGraphCacheRelationshipStorePath sets the file path for persisting learned relationships.
	// When set, uses a file-based store instead of the default ConfigMap store.
	EnvGraphCacheRelationshipStorePath = "ARGOCD_RELATIONSHIP_STORE_PATH"

	// EnvCustomRelationshipsFile sets the path to a YAML file defining custom
	// parent→child type relationships.
	EnvCustomRelationshipsFile = "ARGOCD_CUSTOM_RELATIONSHIPS_FILE"
)

// CacheFactoryConfig contains configuration for the cache factory.
// Fields are grouped by which cache implementation uses them.
type CacheFactoryConfig struct {
	// Shared dependencies (used by both traditional and graph cache)
	DB               db.ArgoDB
	SettingsMgr      *settings.SettingsManager
	MetricsServer    *metrics.MetricsServer
	OnObjectUpdated  statecache.ObjectUpdatedHandler
	ClusterSharding  sharding.ClusterShardingCache
	ResourceTracking argo.ResourceTracking

	// Traditional cache only — not used when graph cache is enabled.
	AppInformer cache.SharedIndexInformer

	// Graph cache only — not used when traditional cache is active.
	RepoServerClient      apiclient.Clientset
	ApplicationNamespaces []string
	KubeClient            kubernetes.Interface // For ConfigMap-based relationship persistence
}

// NewLiveStateCache creates a LiveStateCache based on configuration.
// When ARGOCD_ENABLE_GRAPH_CACHE is true (default), the graph-based cache is
// used and the traditional gitops-engine cache is NOT initialized at all.
func NewLiveStateCache(config CacheFactoryConfig) (statecache.LiveStateCache, error) {
	enableGraphCache := env.ParseBoolFromEnv(EnvGraphCacheEnabled, true)

	if !enableGraphCache {
		log.Info("Using traditional gitops-engine cache (graph cache disabled via ARGOCD_ENABLE_GRAPH_CACHE=false)")
		return statecache.NewLiveStateCache(
			config.DB,
			config.AppInformer,
			config.SettingsMgr,
			config.MetricsServer,
			config.OnObjectUpdated,
			config.ClusterSharding,
			config.ResourceTracking,
		), nil
	}

	log.Info("Graph cache enabled — traditional gitops-engine cache will NOT be initialized")

	trackingMethod, err := getTrackingMethod(config.SettingsMgr)
	if err != nil {
		log.Warnf("Failed to get tracking method from settings, using default: %v", err)
		trackingMethod = graphcore.TrackingMethodAnnotationAndLabel
	}

	repoConn, repoClient, err := config.RepoServerClient.NewRepoServerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create repo server client: %w", err)
	}

	populateResourceInfoHandler := createPopulateResourceInfoHandler(config.SettingsMgr, config.ResourceTracking)

	argocdNamespace := ""
	if config.SettingsMgr != nil {
		argocdNamespace = config.SettingsMgr.GetNamespace()
	}

	var promRegistry *prometheus.Registry
	if config.MetricsServer != nil {
		promRegistry = config.MetricsServer.GetRegistry()
	}

	// Set up relationship store
	var relationshipStore graphcore.RelationshipStore
	storeFilePath := env.StringFromEnv(EnvGraphCacheRelationshipStorePath, "")
	if storeFilePath != "" {
		gcLog := textlogger.NewLogger(textlogger.NewConfig()).WithName("graph-cache")
		store, err := graphcore.NewFileStore(graphcore.FileStoreConfig{FilePath: storeFilePath, Log: gcLog})
		if err != nil {
			return nil, fmt.Errorf("failed to create file-based relationship store: %w", err)
		}
		relationshipStore = store
		log.WithField("path", storeFilePath).Info("Using file-based relationship store")
	} else if config.KubeClient != nil {
		gcLog := textlogger.NewLogger(textlogger.NewConfig()).WithName("graph-cache")
		relationshipStore = graphcore.NewConfigMapStore(graphcore.ConfigMapStoreConfig{
			KubeClient: config.KubeClient,
			Namespace:  argocdNamespace,
			Log:        gcLog,
		})
		log.Info("Using ConfigMap-based relationship store")
	}

	customRelFile := env.StringFromEnv(EnvCustomRelationshipsFile, "")

	graphCacheConfig := Config{
		RepoServerClient:            repoClient,
		TrackingMethod:              trackingMethod,
		Namespaces:                  config.ApplicationNamespaces,
		PopulateResourceInfoHandler: populateResourceInfoHandler,
		PrometheusRegistry:          promRegistry,
		RelationshipStore:           relationshipStore,
		CustomRelationshipsFile:     customRelFile,
	}

	adapter := NewGraphLiveStateCache(graphCacheConfig, config.OnObjectUpdated, argocdNamespace, config.SettingsMgr, config.DB, config.ClusterSharding, repoConn)

	log.WithFields(log.Fields{
		"trackingMethod": trackingMethod,
		"namespaces":     config.ApplicationNamespaces,
	}).Info("Graph cache initialized — cluster caches will use selective watches only")

	return adapter, nil
}

// getTrackingMethod determines the tracking method from settings or environment
func getTrackingMethod(settingsMgr *settings.SettingsManager) (graphcore.TrackingMethod, error) {
	envMethod := env.StringFromEnv(EnvGraphCacheTrackingMethod, "")
	if envMethod != "" {
		return parseTrackingMethod(envMethod), nil
	}

	if settingsMgr != nil {
		method, err := settingsMgr.GetTrackingMethod()
		if err != nil {
			log.Warnf("Failed to get tracking method from settings manager: %v", err)
			return graphcore.TrackingMethodAnnotationAndLabel, nil
		}
		switch v1alpha1.TrackingMethod(method) {
		case v1alpha1.TrackingMethodAnnotation:
			return graphcore.TrackingMethodAnnotation, nil
		case v1alpha1.TrackingMethodLabel:
			return graphcore.TrackingMethodLabel, nil
		case v1alpha1.TrackingMethodAnnotationAndLabel:
			return graphcore.TrackingMethodAnnotationAndLabel, nil
		}
	}

	return graphcore.TrackingMethodAnnotationAndLabel, nil
}

// parseTrackingMethod parses a tracking method string
func parseTrackingMethod(method string) graphcore.TrackingMethod {
	switch method {
	case "annotation":
		return graphcore.TrackingMethodAnnotation
	case "label":
		return graphcore.TrackingMethodLabel
	case "annotation+label":
		return graphcore.TrackingMethodAnnotationAndLabel
	default:
		log.Warnf("Unknown tracking method '%s', using default annotation+label", method)
		return graphcore.TrackingMethodAnnotationAndLabel
	}
}

// createPopulateResourceInfoHandler creates a handler that computes health status,
// app name, and other enrichment info for each resource. Settings values are read
// directly from the settings manager, which has its own internal caching via
// ConfigMap informer.
func createPopulateResourceInfoHandler(settingsMgr *settings.SettingsManager, resourceTracking argo.ResourceTracking) PopulateResourceInfoHandler {
	return func(un *unstructured.Unstructured, isRoot bool) (interface{}, bool) {
		res := &statecache.ResourceInfo{}

		customLabels, _ := settingsMgr.GetResourceCustomLabels()
		statecache.PopulateNodeInfo(un, res, customLabels)

		overrides, _ := settingsMgr.GetResourceOverrides()
		res.Health, _ = health.GetResourceHealth(un, lua.ResourceHealthOverrides(overrides))

		if resourceTracking != nil && settingsMgr != nil {
			appInstanceLabelKey, _ := settingsMgr.GetAppInstanceLabelKey()
			trackingMethod, _ := settingsMgr.GetTrackingMethod()
			installationID, _ := settingsMgr.GetInstallationID()
			appName := resourceTracking.GetAppName(un, appInstanceLabelKey, v1alpha1.TrackingMethod(trackingMethod), installationID)
			if isRoot && appName != "" {
				res.AppName = appName
			}
		}

		gvk := un.GroupVersionKind()

		ignoreUpdates, _ := settingsMgr.GetIsIgnoreResourceUpdatesEnabled()
		if ignoreUpdates && statecache.ShouldHashManifest(res.AppName, schema.GroupVersionKind(gvk), un) {
			hash, err := statecache.GenerateManifestHash(un, nil, overrides, normalizers.IgnoreNormalizerOpts{})
			if err == nil {
				res.SetManifestHash(hash)
			}
		}

		return res, res.AppName != "" || gvk.Kind == kube.CustomResourceDefinitionKind
	}
}
