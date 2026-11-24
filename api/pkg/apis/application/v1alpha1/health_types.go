package v1alpha1

// HealthStatusCode is a type which represents possible comprehended health states of an
// application or resource.
//
// These values mirror github.com/argoproj/argo-cd/gitops-engine/v3/pkg/health.HealthStatusCode
// exactly. They are declared here, rather than imported, so that consumers of the Argo CD API
// types do not inherit gitops-engine — and through it k8s.io/kubernetes — in their dependency
// graph. See TestHealthStatusCodeMatchesGitOpsEngine, which fails if the two sets diverge.
type HealthStatusCode string

const (
	// HealthStatusUnknown indicates that health assessment failed and actual health status is unknown.
	HealthStatusUnknown HealthStatusCode = "Unknown"
	// HealthStatusProgressing means that resource is not healthy but still has a chance to reach a healthy state.
	HealthStatusProgressing HealthStatusCode = "Progressing"
	// HealthStatusHealthy means the resource is 100% healthy.
	HealthStatusHealthy HealthStatusCode = "Healthy"
	// HealthStatusSuspended is assigned to resources that are suspended or paused. The typical
	// example is a suspended CronJob.
	HealthStatusSuspended HealthStatusCode = "Suspended"
	// HealthStatusDegraded is used if a resource status indicates failure, or the resource could
	// not reach a healthy state within some timeout.
	HealthStatusDegraded HealthStatusCode = "Degraded"
	// HealthStatusMissing indicates that the resource is missing in the cluster.
	HealthStatusMissing HealthStatusCode = "Missing"
)
