package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizedRepository(t *testing.T) {
	repo := &Repository{
		Repo:                              "https://github.com/argoproj/argo-cd.git",
		Type:                              "git",
		Name:                              "argo-cd",
		Username:                          "admin",
		Password:                          "super-secret-password",
		SSHPrivateKey:                     "-----BEGIN RSA PRIVATE KEY-----",
		BearerToken:                       "eyJhbGciOiJIUzI1NiJ9",
		TLSClientCertData:                 "cert-data",
		TLSClientCertKey:                  "cert-key",
		GCPServiceAccountKey:              "gcp-key",
		GithubAppPrivateKey:               "github-app-key",
		Insecure:                          true,
		EnableLFS:                         true,
		EnableOCI:                         true,
		Proxy:                             "http://proxy:8080",
		NoProxy:                           "localhost",
		Project:                           "default",
		ForceHttpBasicAuth:                true,
		InheritedCreds:                    true,
		GithubAppId:                       12345,
		GithubAppInstallationId:           67890,
		GitHubAppEnterpriseBaseURL:        "https://ghe.example.com/api/v3",
		UseAzureWorkloadIdentity:          true,
		AzureServicePrincipalClientId:     "client-id",
		AzureServicePrincipalClientSecret: "client-secret",
		AzureServicePrincipalTenantId:     "tenant-id",
		Depth:                             1,
	}

	sanitized := repo.Sanitized()

	// Non-sensitive fields must be preserved
	assert.Equal(t, repo.Repo, sanitized.Repo)
	assert.Equal(t, repo.Type, sanitized.Type)
	assert.Equal(t, repo.Name, sanitized.Name)
	assert.Equal(t, repo.Username, sanitized.Username)
	assert.True(t, sanitized.Insecure)
	assert.Equal(t, repo.EnableLFS, sanitized.EnableLFS)
	assert.Equal(t, repo.EnableOCI, sanitized.EnableOCI)
	assert.Equal(t, repo.Proxy, sanitized.Proxy)
	assert.Equal(t, repo.NoProxy, sanitized.NoProxy)
	assert.Equal(t, repo.Project, sanitized.Project)
	assert.Equal(t, repo.ForceHttpBasicAuth, sanitized.ForceHttpBasicAuth)
	assert.Equal(t, repo.InheritedCreds, sanitized.InheritedCreds)
	assert.Equal(t, repo.GithubAppId, sanitized.GithubAppId)
	assert.Equal(t, repo.GithubAppInstallationId, sanitized.GithubAppInstallationId)
	assert.Equal(t, repo.GitHubAppEnterpriseBaseURL, sanitized.GitHubAppEnterpriseBaseURL)
	assert.Equal(t, repo.UseAzureWorkloadIdentity, sanitized.UseAzureWorkloadIdentity)
	assert.Equal(t, repo.AzureServicePrincipalClientId, sanitized.AzureServicePrincipalClientId)
	assert.Equal(t, repo.AzureServicePrincipalTenantId, sanitized.AzureServicePrincipalTenantId)
	assert.Equal(t, repo.Depth, sanitized.Depth)

	// Sensitive fields must be stripped
	assert.Empty(t, sanitized.Password)
	assert.Empty(t, sanitized.SSHPrivateKey)
	assert.Empty(t, sanitized.BearerToken)
	assert.Empty(t, sanitized.TLSClientCertData)
	assert.Empty(t, sanitized.TLSClientCertKey)
	assert.Empty(t, sanitized.GCPServiceAccountKey)
	assert.Empty(t, sanitized.GithubAppPrivateKey)
	assert.Empty(t, sanitized.AzureServicePrincipalClientSecret)
}

func TestSanitizedRepositoryPreservesDepthZero(t *testing.T) {
	// Depth of 0 means full clone; verify it's preserved (zero value)
	repo := &Repository{
		Repo:  "https://github.com/argoproj/argo-cd.git",
		Depth: 0,
	}

	sanitized := repo.Sanitized()
	assert.Equal(t, int64(0), sanitized.Depth)
}

func TestRepository_Normalize(t *testing.T) {
	tests := []struct {
		name     string
		repo     Repository
		wantType string
	}{
		{
			name:     "OCI URL with empty type defaults to oci",
			repo:     Repository{Repo: "oci://example.com/foo"},
			wantType: "oci",
		},
		{
			name:     "HTTPS URL with empty type defaults to git",
			repo:     Repository{Repo: "https://example.com/foo/bar.git"},
			wantType: "git",
		},
		{
			name:     "SSH URL with empty type defaults to git",
			repo:     Repository{Repo: "ssh://git@example.com/foo.git"},
			wantType: "git",
		},
		{
			name:     "Explicit git type is preserved on HTTPS URL",
			repo:     Repository{Repo: "https://example.com/foo/bar.git", Type: "git"},
			wantType: "git",
		},
		{
			name:     "Explicit git type is preserved on SSH URL",
			repo:     Repository{Repo: "ssh://git@example.com/foo.git", Type: "git"},
			wantType: "git",
		},
		{
			name:     "Explicit helm type is preserved on HTTPS URL",
			repo:     Repository{Repo: "https://charts.example.com", Type: "helm"},
			wantType: "helm",
		},
		{
			name:     "Explicit oci type is preserved on OCI URL",
			repo:     Repository{Repo: "oci://example.com/foo", Type: "oci"},
			wantType: "oci",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.repo.Normalize()
			assert.Equal(t, tt.wantType, got.Type)
		})
	}
}
