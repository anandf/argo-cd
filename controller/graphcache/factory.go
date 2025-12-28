package graphcache

import (
	"fmt"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
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
// depending on the ARGOCD_ENABLE_GRAPH_CACHE environment variable
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

	// Create graph cache config
	graphCacheConfig := Config{
		DynamicClient:    config.DynamicClient,
		DiscoveryClient:  config.DiscoveryClient,
		RepoServerClient: repoClient,
		TrackingMethod:   trackingMethod,
		Namespaces:       config.ApplicationNamespaces,
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
	adapter := NewGraphLiveStateCache(graphCacheConfig, store)

	log.Info("Graph cache initialized successfully")
	log.WithFields(log.Fields{
		"trackingMethod": trackingMethod,
		"namespaces":     config.ApplicationNamespaces,
		"persistence":    store != nil,
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
