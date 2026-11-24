// Package helmurl holds Helm repository URL predicates that carry no dependency on the Helm
// client, so that the Argo CD API types can use them without pulling in the Helm machinery.
package helmurl

import (
	"net/url"
	"strings"
)

func IsHelmOciRepo(repoURL string) bool {
	if repoURL == "" || strings.Contains(repoURL, "://") {
		return false
	}
	// A scheme-less registry reference such as "demo.goharbor.io:8080" or
	// "192.168.100.150:1843" cannot be parsed as a URL on its own, so prepend a
	// dummy scheme and require a host, the same way oras-go validates a registry
	// (registry.Reference.ValidateRegistry).
	parsed, err := url.ParseRequestURI("dummy://" + repoURL)
	return err == nil && parsed.Host != ""
}
