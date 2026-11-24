// Package actions holds the RBAC resource and action identifiers, and the set of resources that
// can be scoped to a project.
//
// They are plain strings, split out of util/rbac so that the Argo CD API types can validate
// AppProject role policies without pulling in the Casbin enforcer.
package actions

const (
	// please add new items to Resources
	ResourceClusters          = "clusters"
	ResourceProjects          = "projects"
	ResourceApplications      = "applications"
	ResourceApplicationSets   = "applicationsets"
	ResourceRepositories      = "repositories"
	ResourceWriteRepositories = "write-repositories"
	ResourceCertificates      = "certificates"
	ResourceAccounts          = "accounts"
	ResourceGPGKeys           = "gpgkeys"
	ResourceLogs              = "logs"
	ResourceExec              = "exec"
	ResourceExtensions        = "extensions"

	// please add new items to Actions
	ActionGet      = "get"
	ActionCreate   = "create"
	ActionUpdate   = "update"
	ActionDelete   = "delete"
	ActionSync     = "sync"
	ActionOverride = "override"
	ActionAction   = "action"
	ActionInvoke   = "invoke"
	ActionRollback = "rollback"
)

var ProjectScoped = map[string]bool{
	ResourceApplications:    true,
	ResourceApplicationSets: true,
	ResourceLogs:            true,
	ResourceExec:            true,
	ResourceClusters:        true,
	ResourceRepositories:    true,
}
