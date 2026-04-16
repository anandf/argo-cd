package graphcache

import (
	"fmt"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/health"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	statecache "github.com/argoproj/argo-cd/v3/controller/cache"
	"github.com/argoproj/argo-cd/v3/controller/metrics"
	"github.com/argoproj/argo-cd/v3/controller/sharding"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/util/argo"
	"github.com/argoproj/argo-cd/v3/util/db"
	"github.com/argoproj/argo-cd/v3/util/env"
	"github.com/argoproj/argo-cd/v3/util/lua"
	"github.com/argoproj/argo-cd/v3/util/settings"
)

const (
	// EnvGraphCacheEnabled enables the graph-based cache
	EnvGraphCacheEnabled = "ARGOCD_ENABLE_GRAPH_CACHE"

	// EnvGraphCachePersistenceEnabled enables persistence for graph cache
	EnvGraphCachePersistenceEnabled = "ARGOCD_GRAPH_CACHE_PERSISTENCE_ENABLED"

	// EnvGraphCacheTrackingMethod sets the tracking method for graph cache
	EnvGraphCacheTrackingMethod = "ARGOCD_GRAPH_CACHE_TRACKING_METHOD"
)

// CacheFactoryConfig contains configuration for cache factory
type CacheFactoryConfig struct {
	// Traditional cache dependencies
	DB               db.ArgoDB
	AppInformer      cache.SharedIndexInformer
	SettingsMgr      *settings.SettingsManager
	MetricsServer    *metrics.MetricsServer
	OnObjectUpdated  statecache.ObjectUpdatedHandler
	ClusterSharding  sharding.ClusterShardingCache
	ResourceTracking argo.ResourceTracking

	// Graph cache dependencies
	KubeClientset         kubernetes.Interface
	DynamicClient         dynamic.Interface
	DiscoveryClient       discovery.DiscoveryInterface
	RepoServerClient      apiclient.Clientset
	ApplicationNamespaces []string
	RedisClient           *redis.Client
}

// NewLiveStateCache creates a LiveStateCache based on configuration
// It will create either a traditional gitops-engine cache or a graph-based cache
// depending on the ARGOCD_ENABLE_GRAPH_CACHE environment variable.
//
// Rollout is controlled by environment variables:
//   - ARGOCD_ENABLE_GRAPH_CACHE: master switch (must be "true" to enable)
//   - ARGOCD_GRAPH_CACHE_ROLLOUT_STRATEGY: "all" (default), "percentage", or "allowlist"
//   - ARGOCD_GRAPH_CACHE_ROLLOUT_PERCENTAGE: 0-100, used with "percentage" strategy
//   - ARGOCD_GRAPH_CACHE_CLUSTER_ALLOWLIST: comma-separated server URLs for "allowlist" strategy
func NewLiveStateCache(config CacheFactoryConfig) (statecache.LiveStateCache, error) {
	// Check if graph cache is enabled
	enableGraphCache := env.ParseBoolFromEnv(EnvGraphCacheEnabled, false)

	if !enableGraphCache {
		log.Info("Using traditional gitops-engine cache")
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

	log.Info("Graph cache enabled - initializing graph-based cache")

	// Load rollout configuration for gradual rollout support
	rolloutConfig := NewRolloutConfigFromEnv()

	// Get tracking method from settings or env var
	trackingMethod, err := getTrackingMethod(config.SettingsMgr)
	if err != nil {
		log.Warnf("Failed to get tracking method from settings, using default: %v", err)
		trackingMethod = TrackingMethodAnnotationAndLabel
	}

	_, repoClient, err := config.RepoServerClient.NewRepoServerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create repo server client: %w", err)
	}

	// Create PopulateResourceInfoHandler matching traditional cache behavior.
	// This computes health status, app name, and determines whether to cache the manifest.
	populateResourceInfoHandler := createPopulateResourceInfoHandler(config.SettingsMgr, config.ResourceTracking)

	// Create graph cache config
	graphCacheConfig := Config{
		DynamicClient:               config.DynamicClient,
		DiscoveryClient:             config.DiscoveryClient,
		RepoServerClient:            repoClient,
		TrackingMethod:              trackingMethod,
		Namespaces:                  config.ApplicationNamespaces,
		PopulateResourceInfoHandler: populateResourceInfoHandler,
	}

	var store GraphStore
	if env.ParseBoolFromEnv(EnvGraphCachePersistenceEnabled, false) {
		if config.RedisClient != nil {
			log.Info("Initializing Redis store for graph cache persistence")
			store = NewRedisStore(RedisStoreConfig{
				Client: config.RedisClient,
				Key:    "argocd:graph-cache", // Base key
			})
		} else {
			log.Warn("Graph cache persistence enabled but Redis client is missing")
		}
	}

	// Create and return adapter
	// Graph caches for clusters will be created lazily
	argocdNamespace := ""
	if config.SettingsMgr != nil {
		argocdNamespace = config.SettingsMgr.GetNamespace()
	}
	adapter := NewGraphLiveStateCache(graphCacheConfig, store, config.OnObjectUpdated, argocdNamespace, config.SettingsMgr)
	adapter.rolloutConfig = rolloutConfig

	log.Info("Graph cache initialized successfully")
	log.WithFields(log.Fields{
		"trackingMethod":  trackingMethod,
		"namespaces":      config.ApplicationNamespaces,
		"persistence":     store != nil,
		"rolloutStrategy": rolloutConfig.Strategy,
		"rolloutPct":      rolloutConfig.Percentage,
	}).Info("Graph cache configuration")

	return adapter, nil
}

// getTrackingMethod determines the tracking method from settings or environment
func getTrackingMethod(settingsMgr *settings.SettingsManager) (TrackingMethod, error) {
	// Try environment variable first
	envMethod := env.StringFromEnv(EnvGraphCacheTrackingMethod, "")
	if envMethod != "" {
		return parseTrackingMethod(envMethod), nil
	}

	// Try settings manager
	if settingsMgr != nil {
		method, err := settingsMgr.GetTrackingMethod()
		if err != nil {
			log.Warnf("Failed to get tracking method from settings manager: %v", err)
			return TrackingMethodAnnotationAndLabel, nil
		}
		switch v1alpha1.TrackingMethod(method) {
		case v1alpha1.TrackingMethodAnnotation:
			return TrackingMethodAnnotation, nil
		case v1alpha1.TrackingMethodLabel:
			return TrackingMethodLabel, nil
		case v1alpha1.TrackingMethodAnnotationAndLabel:
			return TrackingMethodAnnotationAndLabel, nil
		}
	}

	// Default
	return TrackingMethodAnnotationAndLabel, nil
}

// parseTrackingMethod parses a tracking method string
func parseTrackingMethod(method string) TrackingMethod {
	switch method {
	case "annotation":
		return TrackingMethodAnnotation
	case "label":
		return TrackingMethodLabel
	case "annotation+label":
		return TrackingMethodAnnotationAndLabel
	default:
		log.Warnf("Unknown tracking method '%s', using default annotation+label", method)
		return TrackingMethodAnnotationAndLabel
	}
}

// createPopulateResourceInfoHandler creates a handler that computes health status,
// app name, and other enrichment info for each resource. This mirrors the handler
// used by the traditional gitops-engine cache in controller/cache/cache.go.
func createPopulateResourceInfoHandler(settingsMgr *settings.SettingsManager, resourceTracking argo.ResourceTracking) PopulateResourceInfoHandler {
	return func(un *unstructured.Unstructured, isRoot bool) (interface{}, bool) {
		res := &statecache.ResourceInfo{}

		// Populate node info (images, networking, pod info, custom labels)
		var customLabels []string
		if settingsMgr != nil {
			var err error
			customLabels, err = settingsMgr.GetResourceCustomLabels()
			if err != nil {
				log.Warnf("Failed to get custom labels: %v", err)
			}
		}
		statecache.PopulateNodeInfo(un, res, customLabels)

		// Compute health status using resource overrides (Lua health checks)
		var healthOverride health.HealthOverride
		if settingsMgr != nil {
			resourceOverrides, err := settingsMgr.GetResourceOverrides()
			if err != nil {
				log.Warnf("Failed to get resource overrides for health check: %v", err)
			} else {
				healthOverride = lua.ResourceHealthOverrides(resourceOverrides)
			}
		}
		res.Health, _ = health.GetResourceHealth(un, healthOverride)

		// Determine app name from tracking info
		if resourceTracking != nil && settingsMgr != nil {
			appInstanceLabelKey, err := settingsMgr.GetAppInstanceLabelKey()
			if err != nil {
				log.Warnf("Failed to get app instance label key: %v", err)
			}
			trackingMethod, err := settingsMgr.GetTrackingMethod()
			if err != nil {
				log.Warnf("Failed to get tracking method: %v", err)
			}
			installationID, err := settingsMgr.GetInstallationID()
			if err != nil {
				log.Warnf("Failed to get installation ID: %v", err)
			}

			appName := resourceTracking.GetAppName(un, appInstanceLabelKey, v1alpha1.TrackingMethod(trackingMethod), installationID)
			if isRoot && appName != "" {
				res.AppName = appName
			}
		}

		gvk := un.GroupVersionKind()

		// Cache manifest for managed resources and CRDs (CRDs aren't labeled but needed for diff)
		return res, res.AppName != "" || gvk.Kind == kube.CustomResourceDefinitionKind
	}
}
