package graphcache

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/argoproj/argo-cd/v3/util/env"
)

const (
	// EnvGraphCacheRolloutStrategy controls the rollout strategy for graph cache.
	// Values: "all" (default when enabled), "percentage", "allowlist"
	EnvGraphCacheRolloutStrategy = "ARGOCD_GRAPH_CACHE_ROLLOUT_STRATEGY"

	// EnvGraphCacheRolloutPercentage sets the percentage of clusters using graph cache (0-100).
	// Only used when strategy is "percentage".
	EnvGraphCacheRolloutPercentage = "ARGOCD_GRAPH_CACHE_ROLLOUT_PERCENTAGE"

	// EnvGraphCacheClusterAllowlist is a comma-separated list of cluster server URLs
	// that should use the graph cache. Only used when strategy is "allowlist".
	EnvGraphCacheClusterAllowlist = "ARGOCD_GRAPH_CACHE_CLUSTER_ALLOWLIST"

	// RolloutStrategyAll enables graph cache for all clusters.
	RolloutStrategyAll = "all"
	// RolloutStrategyPercentage enables graph cache for a percentage of clusters.
	RolloutStrategyPercentage = "percentage"
	// RolloutStrategyAllowlist enables graph cache only for explicitly listed clusters.
	RolloutStrategyAllowlist = "allowlist"
)

// RolloutConfig controls which clusters use graph cache vs traditional cache.
type RolloutConfig struct {
	// Strategy determines how clusters are selected.
	// "all" - all clusters use graph cache (default when enabled)
	// "percentage" - deterministic percentage of clusters use graph cache
	// "allowlist" - only explicitly listed clusters use graph cache
	Strategy string

	// Percentage of clusters to use graph cache (0-100).
	// Only used with "percentage" strategy.
	Percentage int

	// Allowlist of cluster server URLs that should use graph cache.
	// Only used with "allowlist" strategy.
	Allowlist map[string]bool

	lock sync.RWMutex
}

// NewRolloutConfigFromEnv creates a RolloutConfig from environment variables.
func NewRolloutConfigFromEnv() *RolloutConfig {
	strategy := env.StringFromEnv(EnvGraphCacheRolloutStrategy, RolloutStrategyAll)
	percentage := env.ParseNumFromEnv(EnvGraphCacheRolloutPercentage, 100, 0, 100)

	allowlistStr := env.StringFromEnv(EnvGraphCacheClusterAllowlist, "")
	allowlist := make(map[string]bool)
	if allowlistStr != "" {
		for _, server := range strings.Split(allowlistStr, ",") {
			server = strings.TrimSpace(server)
			if server != "" {
				allowlist[server] = true
			}
		}
	}

	config := &RolloutConfig{
		Strategy:   strategy,
		Percentage: percentage,
		Allowlist:  allowlist,
	}

	log.WithFields(log.Fields{
		"component":  "graph-cache",
		"strategy":   strategy,
		"percentage": percentage,
		"allowlist":  len(allowlist),
	}).Info("Rollout configuration loaded")

	return config
}

// ShouldUseGraphCache determines if a specific cluster should use the graph cache.
// The decision is deterministic based on the cluster server URL, so the same
// cluster always gets the same result for a given percentage.
func (rc *RolloutConfig) ShouldUseGraphCache(clusterServer string) bool {
	rc.lock.RLock()
	defer rc.lock.RUnlock()

	switch rc.Strategy {
	case RolloutStrategyAll:
		return true

	case RolloutStrategyPercentage:
		return rc.isInPercentage(clusterServer)

	case RolloutStrategyAllowlist:
		return rc.Allowlist[clusterServer]

	default:
		log.WithFields(log.Fields{
			"component": "graph-cache",
			"strategy":  rc.Strategy,
		}).Warn("Unknown rollout strategy, defaulting to all")
		return true
	}
}

// isInPercentage uses a deterministic hash to decide if a cluster falls within
// the configured percentage. The same cluster URL always produces the same result.
func (rc *RolloutConfig) isInPercentage(clusterServer string) bool {
	if rc.Percentage >= 100 {
		return true
	}
	if rc.Percentage <= 0 {
		return false
	}

	h := sha256.Sum256([]byte(clusterServer))
	hashVal := binary.BigEndian.Uint32(h[:4])
	bucket := hashVal % 100

	return int(bucket) < rc.Percentage
}

// UpdateStrategy updates the rollout strategy at runtime.
func (rc *RolloutConfig) UpdateStrategy(strategy string) {
	rc.lock.Lock()
	defer rc.lock.Unlock()
	rc.Strategy = strategy
}

// UpdatePercentage updates the rollout percentage at runtime.
func (rc *RolloutConfig) UpdatePercentage(percentage int) {
	rc.lock.Lock()
	defer rc.lock.Unlock()
	if percentage < 0 {
		percentage = 0
	}
	if percentage > 100 {
		percentage = 100
	}
	rc.Percentage = percentage
}

// AddToAllowlist adds a cluster server URL to the allowlist.
func (rc *RolloutConfig) AddToAllowlist(server string) {
	rc.lock.Lock()
	defer rc.lock.Unlock()
	rc.Allowlist[server] = true
}

// RemoveFromAllowlist removes a cluster server URL from the allowlist.
func (rc *RolloutConfig) RemoveFromAllowlist(server string) {
	rc.lock.Lock()
	defer rc.lock.Unlock()
	delete(rc.Allowlist, server)
}
