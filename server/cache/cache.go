package cache

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/spf13/cobra"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	cacheutil "github.com/argoproj/argo-cd/v3/util/cache"
	appstatecache "github.com/argoproj/argo-cd/v3/util/cache/appstate"
	"github.com/argoproj/argo-cd/v3/util/env"
)

var ErrCacheMiss = appstatecache.ErrCacheMiss

type Cache struct {
	cache                           *appstatecache.Cache
	connectionStatusCacheExpiration time.Duration
	oidcCacheExpiration             time.Duration
}

func NewCache(
	cache *appstatecache.Cache,
	connectionStatusCacheExpiration time.Duration,
	oidcCacheExpiration time.Duration,
) *Cache {
	return &Cache{cache, connectionStatusCacheExpiration, oidcCacheExpiration}
}

func AddCacheFlagsToCmd(cmd *cobra.Command, opts ...cacheutil.Options) func() (*Cache, error) {
	var connectionStatusCacheExpiration time.Duration
	var oidcCacheExpiration time.Duration
	var loginAttemptsExpiration time.Duration

	cmd.Flags().DurationVar(&connectionStatusCacheExpiration, "connection-status-cache-expiration", env.ParseDurationFromEnv("ARGOCD_SERVER_CONNECTION_STATUS_CACHE_EXPIRATION", 1*time.Hour, 0, math.MaxInt64), "Cache expiration for cluster/repo connection status")
	cmd.Flags().DurationVar(&oidcCacheExpiration, "oidc-cache-expiration", env.ParseDurationFromEnv("ARGOCD_SERVER_OIDC_CACHE_EXPIRATION", 3*time.Minute, 0, math.MaxInt64), "Cache expiration for OIDC state")
	cmd.Flags().DurationVar(&loginAttemptsExpiration, "login-attempts-expiration", env.ParseDurationFromEnv("ARGOCD_SERVER_LOGIN_ATTEMPTS_EXPIRATION", 24*time.Hour, 0, math.MaxInt64), "Cache expiration for failed login attempts. DEPRECATED: this flag is unused and will be removed in a future version.")

	fn := appstatecache.AddCacheFlagsToCmd(cmd, opts...)

	return func() (*Cache, error) {
		cache, err := fn()
		if err != nil {
			return nil, err
		}

		return NewCache(cache, connectionStatusCacheExpiration, oidcCacheExpiration), nil
	}
}

func (c *Cache) GetAppResourcesTree(appName string, res *appv1.ApplicationTree) error {
	return c.cache.GetAppResourcesTree(appName, res)
}

func (c *Cache) OnAppResourcesTreeChanged(ctx context.Context, appName string, callback func() error) error {
	return c.cache.OnAppResourcesTreeChanged(ctx, appName, callback)
}

func (c *Cache) GetAppManagedResources(appName string, res *[]*appv1.ResourceDiff) error {
	return c.cache.GetAppManagedResources(appName, res)
}

func (c *Cache) SetRepoConnectionState(repo string, project string, state *appv1.ConnectionState) error {
	return c.cache.SetItem(repoConnectionStateKey(repo, project), &state, c.connectionStatusCacheExpiration, state == nil)
}

func repoConnectionStateKey(repo string, project string) string {
	return fmt.Sprintf("repo|%s|%s|connection-state", repo, project)
}

func (c *Cache) GetRepoConnectionState(repo string, project string) (appv1.ConnectionState, error) {
	res := appv1.ConnectionState{}
	err := c.cache.GetItem(repoConnectionStateKey(repo, project), &res)
	return res, err
}

func (c *Cache) GetClusterInfo(server string, res *appv1.ClusterInfo) error {
	return c.cache.GetClusterInfo(server, res)
}

func (c *Cache) SetClusterInfo(server string, res *appv1.ClusterInfo) error {
	return c.cache.SetClusterInfo(server, res)
}

func (c *Cache) GetCache() *cacheutil.Cache {
	return c.cache.Cache
}

// AppListSummary holds aggregate counts for the application list.
type AppListSummary struct {
	TotalCount   int64            `json:"totalCount"`
	HealthCounts map[string]int64 `json:"healthCounts"`
	SyncCounts   map[string]int64 `json:"syncCounts"`
}

const (
	appListCacheExpiration    = 15 * time.Second
	appSummaryCacheExpiration = 10 * time.Second
)

// allowedAppNamesKey builds the cache key for the RBAC-filtered list of
// application names visible to a specific user.  The resourceVersion is
// included so that any informer update naturally invalidates the cache.
// The policyVersion is included so that RBAC policy changes (in argocd-rbac-cm)
// immediately invalidate the cache, closing the window where stale permissions
// could be served.
func allowedAppNamesKey(userHash, resourceVersion string, policyVersion uint64, namespace, selector string, projects []string) string {
	return fmt.Sprintf("app-list|%s|%s|%d|%s|%s|%s", userHash, resourceVersion, policyVersion, namespace, selector, strings.Join(projects, ","))
}

// appListSummaryKey builds the cache key for application list summary counts.
func appListSummaryKey(userHash, resourceVersion string, policyVersion uint64, namespace, selector string, projects []string) string {
	return fmt.Sprintf("app-summary|%s|%s|%d|%s|%s|%s", userHash, resourceVersion, policyVersion, namespace, selector, strings.Join(projects, ","))
}

// GetAllowedAppNames returns the cached sorted list of application qualified
// names that a given user is allowed to see.
func (c *Cache) GetAllowedAppNames(userHash, resourceVersion string, policyVersion uint64, namespace, selector string, projects []string) ([]string, error) {
	var names []string
	err := c.cache.GetItem(allowedAppNamesKey(userHash, resourceVersion, policyVersion, namespace, selector, projects), &names)
	return names, err
}

// SetAllowedAppNames caches the sorted list of application qualified names
// that a given user is allowed to see.
func (c *Cache) SetAllowedAppNames(userHash, resourceVersion string, policyVersion uint64, namespace, selector string, projects []string, names []string) error {
	return c.cache.SetItem(allowedAppNamesKey(userHash, resourceVersion, policyVersion, namespace, selector, projects), names, appListCacheExpiration, false)
}

// GetAppListSummary returns the cached application list summary counts for a user.
func (c *Cache) GetAppListSummary(userHash, resourceVersion string, policyVersion uint64, namespace, selector string, projects []string) (AppListSummary, error) {
	var summary AppListSummary
	err := c.cache.GetItem(appListSummaryKey(userHash, resourceVersion, policyVersion, namespace, selector, projects), &summary)
	return summary, err
}

// SetAppListSummary caches the application list summary counts for a user.
func (c *Cache) SetAppListSummary(userHash, resourceVersion string, policyVersion uint64, namespace, selector string, projects []string, summary AppListSummary) error {
	return c.cache.SetItem(appListSummaryKey(userHash, resourceVersion, policyVersion, namespace, selector, projects), summary, appSummaryCacheExpiration, false)
}
