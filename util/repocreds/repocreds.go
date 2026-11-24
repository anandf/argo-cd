// Package repocreds builds the credential objects used to authenticate against the Git, Helm and
// OCI repositories described by a v1alpha1.Repository.
//
// These were previously methods on v1alpha1.Repository. They live here because constructing
// credentials requires util/git, util/helm, util/oci, util/cert and util/workloadidentity —
// between them go-git, oras-go, the Azure SDK and MSAL. Keeping that out of the API types lets
// consumers depend on the Argo CD CRDs without inheriting any of it.
package repocreds

import (
	"net/url"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/argoproj/argo-cd/api/v3/pkg/apis/application/v1alpha1"

	"github.com/argoproj/argo-cd/v3/util/cert"
	"github.com/argoproj/argo-cd/v3/util/git"
	"github.com/argoproj/argo-cd/v3/util/helm"
	"github.com/argoproj/argo-cd/v3/util/oci"
	"github.com/argoproj/argo-cd/v3/util/workloadidentity"
)

// GetGitCreds returns the credentials from a repository configuration used to authenticate at a Git repository
func GetGitCreds(repo *v1alpha1.Repository, store git.CredsStore) git.Creds {
	if repo == nil {
		return git.NopCreds{}
	}
	if repo.Password != "" || repo.BearerToken != "" {
		return git.NewHTTPSCreds(repo.Username, repo.Password, repo.BearerToken, repo.TLSClientCertData, repo.TLSClientCertKey, repo.IsInsecure(), store, repo.ForceHttpBasicAuth)
	}
	if repo.SSHPrivateKey != "" {
		return git.NewSSHCreds(repo.SSHPrivateKey, getCAPath(repo.Repo), repo.IsInsecure(), repo.Proxy)
	}
	if repo.GithubAppPrivateKey != "" && repo.GithubAppId != 0 { // Promoter MVP: remove github-app-installation-id check since it is no longer a required field
		return git.NewGitHubAppCreds(repo.GithubAppId, repo.GithubAppInstallationId, repo.GithubAppPrivateKey, repo.GitHubAppEnterpriseBaseURL, repo.TLSClientCertData, repo.TLSClientCertKey, repo.IsInsecure(), repo.Proxy, repo.NoProxy, store, repo.Repo)
	}
	if repo.GCPServiceAccountKey != "" {
		return git.NewGoogleCloudCreds(repo.GCPServiceAccountKey, store)
	}
	if repo.UseAzureWorkloadIdentity {
		return git.NewAzureWorkloadIdentityCreds(store, workloadidentity.NewWorkloadIdentityTokenProvider())
	}
	if repo.AzureServicePrincipalClientId != "" && repo.AzureServicePrincipalClientSecret != "" && repo.AzureServicePrincipalTenantId != "" {
		creds := git.NewAzureServicePrincipalCreds(repo.AzureServicePrincipalTenantId, repo.AzureServicePrincipalClientId, repo.AzureServicePrincipalClientSecret, store).
			WithActiveDirectoryEndpoint(repo.AzureActiveDirectoryEndpoint).
			WithClientCert(repo.TLSClientCertData, repo.TLSClientCertKey).
			WithProxy(repo.Proxy).
			WithNoProxy(repo.NoProxy)
		return creds
	}
	return git.NopCreds{}
}

// GetHelmCreds returns the credentials from a repository configuration used to authenticate a Helm repository
func GetHelmCreds(repo *v1alpha1.Repository) helm.Creds {
	if repo.UseAzureWorkloadIdentity {
		return helm.NewAzureWorkloadIdentityCreds(
			repo.Repo,
			getCAPath(repo.Repo),
			[]byte(repo.TLSClientCertData),
			[]byte(repo.TLSClientCertKey),
			repo.Insecure,
			workloadidentity.NewWorkloadIdentityTokenProvider(),
		)
	}

	return helm.HelmCreds{
		Username:           repo.Username,
		Password:           repo.Password,
		CAPath:             getCAPath(repo.Repo),
		CertData:           []byte(repo.TLSClientCertData),
		KeyData:            []byte(repo.TLSClientCertKey),
		InsecureSkipVerify: repo.Insecure,
	}
}

// GetOCICreds returns the credentials from a repository configuration used to authenticate an OCI repository
func GetOCICreds(repo *v1alpha1.Repository) oci.Creds {
	return oci.Creds{
		Username:           repo.Username,
		Password:           repo.Password,
		CAPath:             getCAPath(repo.Repo),
		CertData:           []byte(repo.TLSClientCertData),
		KeyData:            []byte(repo.TLSClientCertKey),
		InsecureSkipVerify: repo.Insecure,
		InsecureHTTPOnly:   repo.InsecureOCIForceHttp,
	}
}

func getCAPath(repoURL string) string {
	// For git ssh protocol url without ssh://, url.Parse() will fail to parse.
	// However, no warn log is output since ssh scheme url is a possible format.
	if ok, _ := git.IsSSHURL(repoURL); ok {
		return ""
	}

	hostname := ""
	var parsedURL *url.URL
	var err error
	// Without schema in url, url.Parse() treats the url as differently
	// and may incorrectly parses the hostname if url contains a path or port.
	// To ensure proper parsing, prepend a dummy schema.
	if !strings.Contains(repoURL, "://") {
		parsedURL, err = url.Parse("protocol://" + repoURL)
	} else {
		parsedURL, err = url.Parse(repoURL)
	}
	if err != nil {
		log.Warnf("Could not parse repo URL '%s': %v", repoURL, err)
		return ""
	}

	hostname = parsedURL.Hostname()
	if hostname == "" {
		log.Warnf("Could not get hostname for repository '%s'", repoURL)
		return ""
	}

	caPath, err := cert.GetCertBundlePathForRepository(hostname)
	if err != nil {
		log.Warnf("Could not get cert bundle path for repository '%s': %v", repoURL, err)
		return ""
	}

	return caPath
}
