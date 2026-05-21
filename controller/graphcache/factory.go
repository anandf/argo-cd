package graphcache

import (
	"fmt"
	"sync"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/health"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"

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
	DynamicClient         dynamic.Interface
	DiscoveryClient       discovery.DiscoveryInterface
	RepoServerClient      apiclient.Clientset
	ApplicationNamespaces []string
	RedisClient           *redis.Client
}

// NewLiveStateCache creates a LiveStateCache based on configuration.
// It will create either a traditional gitops-engine cache or a graph-based cache
// depending on the ARGOCD_ENABLE_GRAPH_CACHE environment variable.
func NewLiveStateCache(config CacheFactoryConfig) (statecache.LiveStateCache, error) {
	enableGraphCache := env.ParseBoolFromEnv(EnvGraphCacheEnabled, true)

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

	trackingMethod, err := getTrackingMethod(config.SettingsMgr)
	if err != nil {
		log.Warnf("Failed to get tracking method from settings, using default: %v", err)
		trackingMethod = TrackingMethodAnnotationAndLabel
	}

	repoConn, repoClient, err := config.RepoServerClient.NewRepoServerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create repo server client: %w", err)
	}

	populateResourceInfoHandler := createPopulateResourceInfoHandler(config.SettingsMgr, config.ResourceTracking)

	var promRegistry *prometheus.Registry
	if config.MetricsServer != nil {
		promRegistry = config.MetricsServer.GetRegistry()
	}

	graphCacheConfig := Config{
		DynamicClient:               config.DynamicClient,
		DiscoveryClient:             config.DiscoveryClient,
		RepoServerClient:            repoClient,
		TrackingMethod:              trackingMethod,
		Namespaces:                  config.ApplicationNamespaces,
		PopulateResourceInfoHandler: populateResourceInfoHandler,
		PrometheusRegistry:          promRegistry,
	}

	var store GraphStore
	if env.ParseBoolFromEnv(EnvGraphCachePersistenceEnabled, false) {
		if config.RedisClient != nil {
			log.Info("Initializing Redis store for graph cache persistence")
			store = NewRedisStore(RedisStoreConfig{
				Client: config.RedisClient,
				Key:    "argocd:graph-cache",
			})
		} else {
			log.Warn("Graph cache persistence enabled but Redis client is missing")
		}
	}

	argocdNamespace := ""
	if config.SettingsMgr != nil {
		argocdNamespace = config.SettingsMgr.GetNamespace()
	}
	adapter := NewGraphLiveStateCache(graphCacheConfig, store, config.OnObjectUpdated, argocdNamespace, config.SettingsMgr, config.DB, config.ClusterSharding, repoConn)

	log.WithFields(log.Fields{
		"trackingMethod": trackingMethod,
		"namespaces":     config.ApplicationNamespaces,
		"persistence":    store != nil,
	}).Info("Graph cache initialized successfully")

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

// settingsCache caches frequently-read settings values to avoid calling the
// settings manager on every resource event. The traditional cache reads these
// once and stores them in a struct; we mirror that pattern with a TTL refresh.
type settingsCache struct {
	settingsMgr      *settings.SettingsManager
	resourceTracking argo.ResourceTracking

	mu                           sync.RWMutex
	customLabels                 []string
	healthOverride               health.HealthOverride
	appInstanceLabelKey          string
	trackingMethod               string
	installationID               string
	resourceOverrides            map[string]v1alpha1.ResourceOverride
	ignoreResourceUpdatesEnabled bool
	resourceRelationshipsRaw     string
	lastRefresh                  time.Time
}

const settingsCacheRefreshInterval = 60 * time.Second

func newSettingsCache(settingsMgr *settings.SettingsManager, resourceTracking argo.ResourceTracking) *settingsCache {
	sc := &settingsCache{
		settingsMgr:      settingsMgr,
		resourceTracking: resourceTracking,
	}
	sc.refresh()
	return sc
}

func (sc *settingsCache) refresh() {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.settingsMgr == nil {
		return
	}

	if labels, err := sc.settingsMgr.GetResourceCustomLabels(); err == nil {
		sc.customLabels = labels
	} else {
		log.Warnf("Failed to refresh custom labels: %v", err)
	}

	if overrides, err := sc.settingsMgr.GetResourceOverrides(); err == nil {
		sc.resourceOverrides = overrides
		sc.healthOverride = lua.ResourceHealthOverrides(overrides)
	} else {
		log.Warnf("Failed to refresh resource overrides: %v", err)
	}

	if key, err := sc.settingsMgr.GetAppInstanceLabelKey(); err == nil {
		sc.appInstanceLabelKey = key
	} else {
		log.Warnf("Failed to refresh app instance label key: %v", err)
	}

	if method, err := sc.settingsMgr.GetTrackingMethod(); err == nil {
		sc.trackingMethod = method
	} else {
		log.Warnf("Failed to refresh tracking method: %v", err)
	}

	if id, err := sc.settingsMgr.GetInstallationID(); err == nil {
		sc.installationID = id
	} else {
		log.Warnf("Failed to refresh installation ID: %v", err)
	}

	if enabled, err := sc.settingsMgr.GetIsIgnoreResourceUpdatesEnabled(); err == nil {
		sc.ignoreResourceUpdatesEnabled = enabled
	} else {
		log.Warnf("Failed to refresh ignore resource updates setting: %v", err)
	}

	if raw, err := sc.settingsMgr.GetResourceRelationshipsRaw(); err == nil {
		sc.resourceRelationshipsRaw = raw
	} else {
		log.Warnf("Failed to refresh resource relationships: %v", err)
	}

	sc.lastRefresh = time.Now()
}

func (sc *settingsCache) ensureFresh() {
	sc.mu.RLock()
	stale := time.Since(sc.lastRefresh) > settingsCacheRefreshInterval
	sc.mu.RUnlock()

	if stale {
		sc.refresh()
	}
}

// snapshot returns a consistent read of all cached values.
type settingsSnapshot struct {
	customLabels                 []string
	healthOverride               health.HealthOverride
	appInstanceLabelKey          string
	trackingMethod               string
	installationID               string
	resourceOverrides            map[string]v1alpha1.ResourceOverride
	ignoreResourceUpdatesEnabled bool
	resourceRelationshipsRaw     string
}

func (sc *settingsCache) snapshot() settingsSnapshot {
	sc.ensureFresh()
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return settingsSnapshot{
		customLabels:                 sc.customLabels,
		healthOverride:               sc.healthOverride,
		appInstanceLabelKey:          sc.appInstanceLabelKey,
		trackingMethod:               sc.trackingMethod,
		installationID:               sc.installationID,
		resourceOverrides:            sc.resourceOverrides,
		ignoreResourceUpdatesEnabled: sc.ignoreResourceUpdatesEnabled,
		resourceRelationshipsRaw:     sc.resourceRelationshipsRaw,
	}
}

// createPopulateResourceInfoHandler creates a handler that computes health status,
// app name, and other enrichment info for each resource. This mirrors the handler
// used by the traditional gitops-engine cache in controller/cache/cache.go.
// Settings values are cached and refreshed periodically rather than fetched per-event.
func createPopulateResourceInfoHandler(settingsMgr *settings.SettingsManager, resourceTracking argo.ResourceTracking) PopulateResourceInfoHandler {
	sc := newSettingsCache(settingsMgr, resourceTracking)

	return func(un *unstructured.Unstructured, isRoot bool) (interface{}, bool) {
		snap := sc.snapshot()
		res := &statecache.ResourceInfo{}

		statecache.PopulateNodeInfo(un, res, snap.customLabels)

		res.Health, _ = health.GetResourceHealth(un, snap.healthOverride)

		if resourceTracking != nil && settingsMgr != nil {
			appName := resourceTracking.GetAppName(un, snap.appInstanceLabelKey, v1alpha1.TrackingMethod(snap.trackingMethod), snap.installationID)
			if isRoot && appName != "" {
				res.AppName = appName
			}
		}

		gvk := un.GroupVersionKind()

		// Compute manifest hash for change detection (mirrors traditional cache behavior).
		if snap.ignoreResourceUpdatesEnabled && statecache.ShouldHashManifest(res.AppName, schema.GroupVersionKind(gvk), un) {
			hash, err := statecache.GenerateManifestHash(un, nil, snap.resourceOverrides, normalizers.IgnoreNormalizerOpts{})
			if err != nil {
				log.Errorf("Failed to generate manifest hash: %v", err)
			} else {
				res.SetManifestHash(hash)
			}
		}

		return res, res.AppName != "" || gvk.Kind == kube.CustomResourceDefinitionKind
	}
}
