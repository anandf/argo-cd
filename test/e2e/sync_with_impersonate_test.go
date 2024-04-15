package e2e

import (
	"context"
	"testing"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	. "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/test/e2e/fixture"
	. "github.com/argoproj/argo-cd/v2/test/e2e/fixture/app"
	"github.com/stretchr/testify/assert"

	//corev1 "k8s.io/api/core/v1"
	//rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSyncWithImpersonateFail(t *testing.T) {
	projectName := "test-project-without-rbac"
	_, err := fixture.AppClientset.ArgoprojV1alpha1().AppProjects(fixture.TestNamespace()).Create(context.Background(),
		&AppProject{
			ObjectMeta: metav1.ObjectMeta{
				Name:      projectName,
				Namespace: fixture.TestNamespace(),
			},
			Spec: AppProjectSpec{
				SourceRepos:      []string{"*"},
				Destinations:     []v1alpha1.ApplicationDestination{{Namespace: "*", Server: "*"}},
				SourceNamespaces: []string{"*"},
			},
		},
		metav1.CreateOptions{})

	assert.NoError(t, err)

	Given(t).
		Path("guestbook").
		When().
		SetParamInSettingConfigMap("application.sync.impersonation.enabled", "true").
		CreateFromFile(func(app *Application) {
			app.Spec.Project = projectName
			app.Spec.SyncPolicy = &SyncPolicy{Automated: &SyncPolicyAutomated{}}
		}).
		Sync().
		Then().
		Expect(SyncStatusIs(SyncStatusCodeOutOfSync))

}