package git

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/argoproj/argo-cd/api/v3/util/git/giturl"
)

// Re-exported from util/git/giturl so that existing callers keep working.
var (
	IsCommitSHA                 = giturl.IsCommitSHA
	IsTruncatedCommitSHA        = giturl.IsTruncatedCommitSHA
	SameURL                     = giturl.SameURL
	NormalizeGitURL             = giturl.NormalizeGitURL
	NormalizeGitURLAllowInvalid = giturl.NormalizeGitURLAllowInvalid
	IsSSHURL                    = giturl.IsSSHURL
	IsHTTPSURL                  = giturl.IsHTTPSURL
	IsHTTPURL                   = giturl.IsHTTPURL
	SanitizeRepoURL             = giturl.SanitizeRepoURL
	SSHHostWithPort             = giturl.SSHHostWithPort
)

// TestRepo tests if a repo exists and is accessible with the given credentials
func TestRepo(repo string, creds Creds, insecure bool, enableLfs bool, proxy string, noProxy string) error {
	client, err := NewClient(repo, creds, insecure, enableLfs, proxy, noProxy)
	if err != nil {
		return fmt.Errorf("unable to initialize git client: %w", err)
	}
	_, err = client.LsRemote("HEAD")
	if err != nil {
		return fmt.Errorf("unable to ls-remote HEAD on repository: %w", err)
	}
	return nil
}

// IsShortRef determines if the supplied revision is a short ref (e.g. "master" instead of "refs/heads/master").
// ref.Name().Short() is an expensive call to be performed in a loop over all refs in a repository, so we want to avoid calling it if we can determine up front that the supplied revision is not a short ref.
// The intention is to optimize for a case where the full ref string is supplied, as comparing the full ref string is cheaper than calling Short() on every ref in the loop.
// If the supplied revision is a short ref, we will compare it with the short version of each ref in the loop.
// If the supplied revision is not a short ref, we will compare it with the full ref string in the loop, which is cheaper than calling Short() on every ref.
// This performance optimization is based on the observation coming from larger repositories where the number of refs can be in the order of tens of thousands,
// and we want to avoid calling Short() on every ref if we can determine up front that the supplied revision is not a short ref.
func IsShortRef(revision string) bool {
	refTentative := plumbing.NewReferenceFromStrings(revision, "dummyHash")
	return refTentative.Name().Short() == revision
}
