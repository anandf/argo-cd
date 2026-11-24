// Package giturl holds Git URL parsing and normalisation helpers.
//
// They are split out of util/git so that callers needing only URL handling — notably the Argo CD
// API types — do not pull in the credential and client machinery, which brings go-git's
// transport stack, go-github and the Azure SDK with it.
package giturl

import (
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// EnsurePrefix idempotently ensures that a base string has a given prefix.
func ensurePrefix(s, prefix string) string {
	if !strings.HasPrefix(s, prefix) {
		s = prefix + s
	}
	return s
}

var (
	commitSHARegex = regexp.MustCompile("^[0-9A-Fa-f]{40}$")
	sshURLRegex    = regexp.MustCompile("^(ssh://)?([^/:]*?)@[^@]+$")
	httpsURLRegex  = regexp.MustCompile("^(https://).*")
	httpURLRegex   = regexp.MustCompile("^(http://).*")
)

// IsCommitSHA returns whether or not a string is a 40 character SHA-1
func IsCommitSHA(sha string) bool {
	return commitSHARegex.MatchString(sha)
}

var truncatedCommitSHARegex = regexp.MustCompile("^[0-9A-Fa-f]{7,}$")

// IsTruncatedCommitSHA returns whether or not a string is a truncated  SHA-1
func IsTruncatedCommitSHA(sha string) bool {
	return truncatedCommitSHARegex.MatchString(sha)
}

// SanitizeRepoURL strips any embedded userinfo (e.g. https://token@host/org/repo) from a
// repository URL so credentials are not leaked when the URL is logged or exported (e.g. as a
// tracing span attribute). If the URL cannot be parsed it is returned unchanged.
func SanitizeRepoURL(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.User == nil {
		return repoURL
	}
	u.User = nil
	return u.String()
}

func SameURL(leftRepo, rightRepo string) bool {
	normalLeft := NormalizeGitURLAllowInvalid(leftRepo)
	normalRight := NormalizeGitURLAllowInvalid(rightRepo)
	return normalLeft != "" && normalRight != "" && normalLeft == normalRight
}

// NormalizeGitURLAllowInvalid is similar to NormalizeGitURL, except returning an original url if the url is invalid.
// Needed to allow a deletion of repos with invalid urls. See https://github.com/argoproj/argo-cd/issues/20921.
func NormalizeGitURLAllowInvalid(repo string) string {
	normalized := NormalizeGitURL(repo)
	if normalized == "" {
		return repo
	}
	return normalized
}

// NormalizeGitURL normalizes a git URL for purposes of comparison, as well as preventing redundant
// local clones (by normalizing various forms of a URL to a consistent location).
// Prefer using SameURL() over this function when possible. This algorithm may change over time
// and should not be considered stable from release to release
func NormalizeGitURL(repo string) string {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if yes, _ := IsSSHURL(repo); yes {
		if !strings.HasPrefix(repo, "ssh://") {
			// We need to replace the first colon in git@server... style SSH URLs with a slash, otherwise
			// net/url.Parse will interpret it incorrectly as the port.
			repo = strings.Replace(repo, ":", "/", 1)
			repo = ensurePrefix(repo, "ssh://")
		}
	}
	repo = strings.TrimSuffix(repo, ".git")
	repoURL, err := url.Parse(repo)
	if err != nil {
		return ""
	}
	normalized := repoURL.String()
	return strings.TrimPrefix(normalized, "ssh://")
}

// IsSSHURL returns true if supplied URL is SSH URL
func IsSSHURL(url string) (bool, string) {
	matches := sshURLRegex.FindStringSubmatch(url)
	if len(matches) > 2 {
		return true, matches[2]
	}
	return false, ""
}

// SSHHostWithPort returns host:port for the given SSH repo URL in the format
// expected by known_hosts lookups (net.JoinHostPort). Returns an empty string
// for non-SSH URLs, URLs that cannot be parsed, or URLs without a host. Port
// defaults to 22 when not present.
func SSHHostWithPort(repoURL string) string {
	if isSSH, _ := IsSSHURL(repoURL); !isSSH {
		return ""
	}
	ep, err := transport.NewEndpoint(repoURL)
	if err != nil || ep.Host == "" {
		return ""
	}
	// transport.NewEndpoint wraps IPv6 hosts in brackets; strip them so that
	// net.JoinHostPort doesn't end up double-bracketing the result.
	host := strings.TrimSuffix(strings.TrimPrefix(ep.Host, "["), "]")
	port := ep.Port
	if port <= 0 {
		port = 22
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// IsHTTPSURL returns true if supplied URL is HTTPS URL
func IsHTTPSURL(url string) bool {
	return httpsURLRegex.MatchString(url)
}

// IsHTTPURL returns true if supplied URL is HTTP URL
func IsHTTPURL(url string) bool {
	return httpURLRegex.MatchString(url)
}
