package cache

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	cacheutil "github.com/argoproj/argo-cd/v3/util/cache"
	appstatecache "github.com/argoproj/argo-cd/v3/util/cache/appstate"
)

type fixtures struct {
	*Cache
}

func newFixtures() *fixtures {
	return &fixtures{NewCache(
		appstatecache.NewCache(
			cacheutil.NewCache(cacheutil.NewInMemoryCache(1*time.Hour)),
			1*time.Minute,
		),
		1*time.Minute,
		1*time.Minute,
	)}
}

func TestCache_GetRepoConnectionState(t *testing.T) {
	cache := newFixtures().Cache
	// cache miss
	_, err := cache.GetRepoConnectionState("my-repo", "")
	assert.Equal(t, ErrCacheMiss, err)
	// populate cache
	err = cache.SetRepoConnectionState("my-repo", "", &ConnectionState{Status: "my-state"})
	require.NoError(t, err)
	// cache miss
	_, err = cache.GetRepoConnectionState("my-repo", "some-project")
	assert.Equal(t, ErrCacheMiss, err)
	// populate cache
	err = cache.SetRepoConnectionState("my-repo", "some-project", &ConnectionState{Status: "my-project-state"})
	require.NoError(t, err)
	// cache hit
	value, err := cache.GetRepoConnectionState("my-repo", "")
	require.NoError(t, err)
	assert.Equal(t, ConnectionState{Status: "my-state"}, value)
	// cache hit
	value, err = cache.GetRepoConnectionState("my-repo", "some-project")
	require.NoError(t, err)
	assert.Equal(t, ConnectionState{Status: "my-project-state"}, value)
}

func TestAddCacheFlagsToCmd(t *testing.T) {
	cache, err := AddCacheFlagsToCmd(&cobra.Command{})()
	require.NoError(t, err)
	assert.Equal(t, 1*time.Hour, cache.connectionStatusCacheExpiration)
	assert.Equal(t, 3*time.Minute, cache.oidcCacheExpiration)
}

func TestCache_AllowedAppNames(t *testing.T) {
	cache := newFixtures().Cache
	projects := []string{"default"}
	var policyV uint64 = 1

	// cache miss
	_, err := cache.GetAllowedAppNames("user1", "100", policyV, "", "", projects)
	assert.Equal(t, ErrCacheMiss, err)

	// populate cache
	names := []string{"argocd/app-a", "argocd/app-b", "argocd/app-c"}
	err = cache.SetAllowedAppNames("user1", "100", policyV, "", "", projects, names)
	require.NoError(t, err)

	// cache hit
	result, err := cache.GetAllowedAppNames("user1", "100", policyV, "", "", projects)
	require.NoError(t, err)
	assert.Equal(t, names, result)

	// different user = cache miss
	_, err = cache.GetAllowedAppNames("user2", "100", policyV, "", "", projects)
	assert.Equal(t, ErrCacheMiss, err)

	// different resourceVersion = cache miss (natural invalidation)
	_, err = cache.GetAllowedAppNames("user1", "101", policyV, "", "", projects)
	assert.Equal(t, ErrCacheMiss, err)

	// different namespace = cache miss
	_, err = cache.GetAllowedAppNames("user1", "100", policyV, "team-a", "", projects)
	assert.Equal(t, ErrCacheMiss, err)

	// different policyVersion = cache miss (RBAC policy changed)
	_, err = cache.GetAllowedAppNames("user1", "100", policyV+1, "", "", projects)
	assert.Equal(t, ErrCacheMiss, err)
}

func TestCache_AppListSummary(t *testing.T) {
	cache := newFixtures().Cache
	projects := []string{"default"}
	var policyV uint64 = 1

	// cache miss
	_, err := cache.GetAppListSummary("user1", "100", policyV, "", "", projects)
	assert.Equal(t, ErrCacheMiss, err)

	// populate cache
	summary := AppListSummary{
		TotalCount:   100,
		HealthCounts: map[string]int64{"Healthy": 80, "Degraded": 20},
		SyncCounts:   map[string]int64{"Synced": 90, "OutOfSync": 10},
	}
	err = cache.SetAppListSummary("user1", "100", policyV, "", "", projects, summary)
	require.NoError(t, err)

	// cache hit
	result, err := cache.GetAppListSummary("user1", "100", policyV, "", "", projects)
	require.NoError(t, err)
	assert.Equal(t, summary, result)

	// different resourceVersion = cache miss
	_, err = cache.GetAppListSummary("user1", "101", policyV, "", "", projects)
	assert.Equal(t, ErrCacheMiss, err)

	// different policyVersion = cache miss (RBAC policy changed)
	_, err = cache.GetAppListSummary("user1", "100", policyV+1, "", "", projects)
	assert.Equal(t, ErrCacheMiss, err)
}
