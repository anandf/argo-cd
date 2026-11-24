package repocreds

import (
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"

	argocdcommon "github.com/argoproj/argo-cd/v3/common"
)

func TestGetCAPath(t *testing.T) {
	temppath := t.TempDir()
	cert, err := os.ReadFile("../../test/fixture/certs/argocd-test-server.crt")
	if err != nil {
		panic(err)
	}
	err = os.WriteFile(path.Join(temppath, "foo.example.com"), cert, 0o666)
	if err != nil {
		panic(err)
	}
	t.Setenv(argocdcommon.EnvVarTLSDataPath, temppath)
	validcert := []string{
		"https://foo.example.com",
		"oci://foo.example.com",
		"foo.example.com",
		"foo.example.com/charts",
		"https://foo.example.com:5000",
		"foo.example.com:5000",
		"foo.example.com:5000/charts",
		"ssh://foo.example.com",
	}
	invalidpath := []string{
		"https://bar.example.com",
		"oci://bar.example.com",
		"bar.example.com",
		"ssh://bar.example.com",
		"git@foo.example.com:organization/reponame.git",
		"ssh://git@foo.example.com:organization/reponame.git",
		"/some/invalid/thing",
		"../another/invalid/thing",
		"./also/invalid",
		"$invalid/as/well",
		"..",
		"://invalid",
	}

	for _, str := range validcert {
		path := getCAPath(str)
		assert.NotEmpty(t, path)
	}
	for _, str := range invalidpath {
		path := getCAPath(str)
		assert.Empty(t, path)
	}
}
