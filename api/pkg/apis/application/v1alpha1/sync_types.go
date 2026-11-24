package v1alpha1

// The enum types in this file mirror
// github.com/argoproj/argo-cd/gitops-engine/v3/pkg/sync/common exactly. They are declared here,
// rather than imported, so that consumers of the Argo CD API types do not inherit gitops-engine —
// and through it k8s.io/kubernetes — in their dependency graph.
//
// Because these are distinct Go types from gitops-engine's, passing a value across that boundary
// needs an explicit conversion. The values themselves are identical, so the conversion is always
// safe. See TestSyncEnumsMatchGitOpsEngine, which fails if the two sets diverge.

// AnnotationDeletionApproved indicates that the deletion of a resource has been approved.
const AnnotationDeletionApproved = "argocd.argoproj.io/deletion-approved"

// SyncPhase is the phase of a sync operation that a resource belongs to.
type SyncPhase string

const (
	SyncPhasePreSync  = "PreSync"
	SyncPhaseSync     = "Sync"
	SyncPhasePostSync = "PostSync"
	SyncPhaseSyncFail = "SyncFail"
)

// OperationPhase is the phase of an operation.
type OperationPhase string

const (
	OperationRunning     OperationPhase = "Running"
	OperationTerminating OperationPhase = "Terminating"
	OperationFailed      OperationPhase = "Failed"
	OperationError       OperationPhase = "Error"
	OperationSucceeded   OperationPhase = "Succeeded"
)

// Completed returns whether the operation has reached a terminal phase.
func (os OperationPhase) Completed() bool {
	switch os {
	case OperationFailed, OperationError, OperationSucceeded:
		return true
	}
	return false
}

// Running returns whether the operation is still in progress.
func (os OperationPhase) Running() bool {
	return os == OperationRunning
}

// Successful returns whether the operation completed successfully.
func (os OperationPhase) Successful() bool {
	return os == OperationSucceeded
}

// Failed returns whether the operation finished in the failed phase. Note that this is narrower
// than "not successful": an errored operation is Completed but neither Successful nor Failed.
func (os OperationPhase) Failed() bool {
	return os == OperationFailed
}

// ResultCode is the result of reconciling a single resource during a sync.
type ResultCode string

const (
	ResultCodeSynced       ResultCode = "Synced"
	ResultCodeSyncFailed   ResultCode = "SyncFailed"
	ResultCodePruned       ResultCode = "Pruned"
	ResultCodePruneSkipped ResultCode = "PruneSkipped"
)

// HookType is the type of a resource hook.
type HookType string

const (
	HookTypePreSync  HookType = "PreSync"
	HookTypeSync     HookType = "Sync"
	HookTypePostSync HookType = "PostSync"
	HookTypeSkip     HookType = "Skip"
	HookTypeSyncFail HookType = "SyncFail"
)

// NewHookType parses a hook type, reporting whether it is a known one.
func NewHookType(t string) (HookType, bool) {
	return HookType(t),
		t == string(HookTypePreSync) ||
			t == string(HookTypeSync) ||
			t == string(HookTypePostSync) ||
			t == string(HookTypeSyncFail) ||
			t == string(HookTypeSkip)
}
