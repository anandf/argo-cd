// Package apicompat guards the boundary between the Argo CD API types and gitops-engine.
//
// The health and sync enums used on Application, AppProject and ApplicationSet are declared in
// pkg/apis/application/v1alpha1 rather than imported from gitops-engine, so that consumers of the
// Argo CD API types do not inherit gitops-engine — and through it k8s.io/kubernetes — in their
// dependency graph.
//
// That leaves two independent definitions of the same enums. Argo CD converts between them at the
// call sites that hand values to gitops-engine, and the compiler enforces that a conversion is
// written. What the compiler cannot catch is the two sets drifting apart in *value*: if
// gitops-engine gains a health status that v1alpha1 lacks, or a constant's string changes on one
// side, every conversion still compiles and the mismatch only shows up at runtime.
//
// These tests exist to turn that runtime failure into a CI failure.
//
// This package deliberately lives in the root module. Putting it inside the API package would add
// gitops-engine to that package's test dependencies, which would put it back in the API module's
// go.mod once the API types move to their own module — defeating the point.
package apicompat

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/health"
	synccommon "github.com/argoproj/argo-cd/gitops-engine/v3/pkg/sync/common"
	kubeutil "github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube"

	"github.com/argoproj/argo-cd/api/v3/pkg/apis/application/v1alpha1"
)

func TestHealthStatusCodeParity(t *testing.T) {
	t.Parallel()

	// Keyed by the v1alpha1 constant, valued by the gitops-engine one. Adding a health status to
	// gitops-engine without adding it here, or to v1alpha1, should fail this test.
	pairs := map[v1alpha1.HealthStatusCode]health.HealthStatusCode{
		v1alpha1.HealthStatusUnknown:     health.HealthStatusUnknown,
		v1alpha1.HealthStatusProgressing: health.HealthStatusProgressing,
		v1alpha1.HealthStatusHealthy:     health.HealthStatusHealthy,
		v1alpha1.HealthStatusSuspended:   health.HealthStatusSuspended,
		v1alpha1.HealthStatusDegraded:    health.HealthStatusDegraded,
		v1alpha1.HealthStatusMissing:     health.HealthStatusMissing,
	}
	for apiVal, engineVal := range pairs {
		assert.Equal(t, string(engineVal), string(apiVal),
			"v1alpha1 and gitops-engine disagree on a health status value; converting between them would silently change meaning")
	}

	assert.Len(t, pairs, 6,
		"gitops-engine's health status set appears to have changed; add the new constant to "+
			"v1alpha1.HealthStatusCode and to this test")
}

func TestOperationPhaseParity(t *testing.T) {
	t.Parallel()

	pairs := map[v1alpha1.OperationPhase]synccommon.OperationPhase{
		v1alpha1.OperationRunning:     synccommon.OperationRunning,
		v1alpha1.OperationTerminating: synccommon.OperationTerminating,
		v1alpha1.OperationFailed:      synccommon.OperationFailed,
		v1alpha1.OperationError:       synccommon.OperationError,
		v1alpha1.OperationSucceeded:   synccommon.OperationSucceeded,
	}
	for apiVal, engineVal := range pairs {
		assert.Equal(t, string(engineVal), string(apiVal))
	}
	assert.Len(t, pairs, 5, "gitops-engine's OperationPhase set changed; update v1alpha1 and this test")
}

// TestOperationPhaseBehaviourParity checks the predicate methods agree, not just the values.
// Failed() is deliberately narrower than "not successful" — an errored operation is Completed but
// neither Successful nor Failed — and that asymmetry is easy to get wrong when reimplementing.
func TestOperationPhaseBehaviourParity(t *testing.T) {
	t.Parallel()

	for _, p := range []string{"Running", "Terminating", "Failed", "Error", "Succeeded"} {
		apiPhase := v1alpha1.OperationPhase(p)
		enginePhase := synccommon.OperationPhase(p)
		assert.Equal(t, enginePhase.Completed(), apiPhase.Completed(), "Completed() disagrees for %q", p)
		assert.Equal(t, enginePhase.Running(), apiPhase.Running(), "Running() disagrees for %q", p)
		assert.Equal(t, enginePhase.Successful(), apiPhase.Successful(), "Successful() disagrees for %q", p)
		assert.Equal(t, enginePhase.Failed(), apiPhase.Failed(), "Failed() disagrees for %q", p)
	}
}

func TestResultCodeParity(t *testing.T) {
	t.Parallel()

	pairs := map[v1alpha1.ResultCode]synccommon.ResultCode{
		v1alpha1.ResultCodeSynced:       synccommon.ResultCodeSynced,
		v1alpha1.ResultCodeSyncFailed:   synccommon.ResultCodeSyncFailed,
		v1alpha1.ResultCodePruned:       synccommon.ResultCodePruned,
		v1alpha1.ResultCodePruneSkipped: synccommon.ResultCodePruneSkipped,
	}
	for apiVal, engineVal := range pairs {
		assert.Equal(t, string(engineVal), string(apiVal))
	}
	assert.Len(t, pairs, 4, "gitops-engine's ResultCode set changed; update v1alpha1 and this test")
}

func TestHookTypeParity(t *testing.T) {
	t.Parallel()

	pairs := map[v1alpha1.HookType]synccommon.HookType{
		v1alpha1.HookTypePreSync:  synccommon.HookTypePreSync,
		v1alpha1.HookTypeSync:     synccommon.HookTypeSync,
		v1alpha1.HookTypePostSync: synccommon.HookTypePostSync,
		v1alpha1.HookTypeSkip:     synccommon.HookTypeSkip,
		v1alpha1.HookTypeSyncFail: synccommon.HookTypeSyncFail,
	}
	for apiVal, engineVal := range pairs {
		assert.Equal(t, string(engineVal), string(apiVal))
	}
	assert.Len(t, pairs, 5, "gitops-engine's HookType set changed; update v1alpha1 and this test")

	// NewHookType must also agree on which strings it accepts.
	for _, in := range []string{"PreSync", "Sync", "PostSync", "Skip", "SyncFail", "Bogus", ""} {
		apiType, apiOK := v1alpha1.NewHookType(in)
		engineType, engineOK := synccommon.NewHookType(in)
		assert.Equal(t, engineOK, apiOK, "NewHookType(%q) validity disagrees", in)
		assert.Equal(t, string(engineType), string(apiType), "NewHookType(%q) value disagrees", in)
	}
}

func TestSyncPhaseParity(t *testing.T) {
	t.Parallel()

	pairs := map[string]string{
		v1alpha1.SyncPhasePreSync:  synccommon.SyncPhasePreSync,
		v1alpha1.SyncPhaseSync:     synccommon.SyncPhaseSync,
		v1alpha1.SyncPhasePostSync: synccommon.SyncPhasePostSync,
		v1alpha1.SyncPhaseSyncFail: synccommon.SyncPhaseSyncFail,
	}
	for apiVal, engineVal := range pairs {
		assert.Equal(t, engineVal, apiVal)
	}
	assert.Len(t, pairs, 4, "gitops-engine's SyncPhase set changed; update v1alpha1 and this test")
}

// TestResourceKeyConvertible asserts the two ResourceKey structs stay layout-compatible, so the
// plain struct conversions Argo CD performs remain valid. Adding or reordering a field on either
// side breaks the conversion, and this test fails to compile when that happens.
func TestResourceKeyConvertible(t *testing.T) {
	t.Parallel()

	engineKey := kubeutil.NewResourceKey("apps", "Deployment", "default", "nginx")
	apiKey := v1alpha1.ResourceKey(engineKey)

	assert.Equal(t, engineKey.Group, apiKey.Group)
	assert.Equal(t, engineKey.Kind, apiKey.Kind)
	assert.Equal(t, engineKey.Namespace, apiKey.Namespace)
	assert.Equal(t, engineKey.Name, apiKey.Name)

	// and back again
	assert.Equal(t, engineKey, kubeutil.ResourceKey(apiKey))
}

func TestAnnotationDeletionApprovedParity(t *testing.T) {
	t.Parallel()

	assert.Equal(t, synccommon.AnnotationDeletionApproved, v1alpha1.AnnotationDeletionApproved)
}
