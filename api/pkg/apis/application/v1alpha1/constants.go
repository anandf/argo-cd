package v1alpha1

// Constants that are part of the Argo CD resource API: annotation keys read off Application and
// ApplicationSet objects, and the default repository type.
//
// They are declared here, rather than imported from the common package, so that consumers of the
// API types do not inherit common's dependencies — go-redis, grpc and client-go among them. The
// common package re-exports these, so there remains a single source of truth.
const (
	// DefaultRepoType is the repository type assumed when a Repository does not set one.
	DefaultRepoType = "git"

	// AnnotationKeyLinkPrefix tells the UI to add an external link icon to the application node.
	AnnotationKeyLinkPrefix = "link.argocd.argoproj.io/"

	// AnnotationApplicationSetRefresh is added when an ApplicationSet is requested to be refreshed
	// by a webhook. The ApplicationSet controller removes it at the end of reconciliation.
	AnnotationApplicationSetRefresh = "argocd.argoproj.io/application-set-refresh"
)
