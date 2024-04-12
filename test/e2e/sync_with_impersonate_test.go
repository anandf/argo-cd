package e2e

import (
	"context"
	"testing"

	//"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	. "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/test/e2e/fixture"
	. "github.com/argoproj/argo-cd/v2/test/e2e/fixture/app"
	"github.com/stretchr/testify/assert"

	//corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSyncWithImpersonateDefaultServiceAccountNoRBAC(t *testing.T) {
	Given(t).
		Path("guestbook").
		When().
		SetParamInSettingConfigMap("application.sync.impersonation.enabled", "true").
		CreateApp().
		Then().
		Expect(SyncStatusIs(SyncStatusCodeOutOfSync))
}

func TestSyncWithImpersonateDefaultServiceAccountWithRBAC(t *testing.T) {
	roleName := "default-sa-role"
	//assert.NoError(t, err)
	Given(t).
		Path("guestbook").
		When().
		SetParamInSettingConfigMap("application.sync.impersonation.enabled", "true").
		CreateApp().
		And(func() {
			err := createRole(roleName, fixture.TestNamespace())
			assert.NoError(t, err)
			err = createRoleBinding(roleName, "default", fixture.TestNamespace())
			assert.NoError(t, err)
		}).
		Sync().
		Then().
		Expect(SyncStatusIs(SyncStatusCodeSynced))
}

func TestSyncWithImpersonateWithApproject(t *testing.T) {
	projectName := "test-project"
	serviceAccountName := "test-account"
	roleName := "test-account-sa-role"

	Given(t).
		SetTrackingMethod("annotation").
		Path("guestbook").
		SetAppNamespace(fixture.AppNamespace()).
		When().
		SetParamInSettingConfigMap("application.sync.impersonation.enabled", "true").
		And(func() {
			createServiceAccount(serviceAccountName, fixture.AppNamespace())
			createAppProject(projectName, fixture.TestNamespace(), serviceAccountName, fixture.AppNamespace())

			createRole(roleName, fixture.AppNamespace())

			createRoleBinding(roleName, serviceAccountName, fixture.AppNamespace())

		}).
		CreateFromFile(func(app *Application) {
			app.Spec.Project = projectName
		}).
		Sync().
		Then().
		Expect(SyncStatusIs(SyncStatusCodeSynced))
}

func createAppProject(name string, namespace string, serviceAccountName string, appNamespace string) error {
	appProject := &AppProject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: AppProjectSpec{
			SourceRepos:      []string{"*"},
			SourceNamespaces: []string{"*"},
			Destinations: []ApplicationDestination{
				{
					Server:    "*",
					Namespace: "*",
				},
			},
			ClusterResourceWhitelist: []metav1.GroupKind{
				{
					Group: "*",
					Kind:  "*",
				},
			},
			DestinationServiceAccounts: []ApplicationDestinationServiceAccount{
				{
					Server:                "*",
					Namespace:             appNamespace,
					DefaultServiceAccount: serviceAccountName,
				},
			},
		},
	}

	_, err := fixture.AppClientset.ArgoprojV1alpha1().AppProjects(namespace).Create(context.Background(), appProject, metav1.CreateOptions{})
	return err
}

func createRole(roleName string, namespace string) error {
	rules := []rbac.PolicyRule{
		{
			APIGroups: []string{"apps", ""},
			Resources: []string{"deployments"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"services"},
			Verbs:     []string{"*"},
		},
	}
	role := &rbac.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleName,
			Namespace: namespace,
		},
		Rules: rules,
	}

	_, err := fixture.KubeClientset.RbacV1().ClusterRoles().Create(context.Background(), role, metav1.CreateOptions{})
	return err
}

func createRoleBinding(roleName, serviceAccountName, namespace string) error {
	roleBinding := &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleName + "-binding",
		},
		Subjects: []rbac.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccountName,
				Namespace: namespace,
			},
		},
		RoleRef: rbac.RoleRef{
			Kind:     "ClusterRole",
			Name:     roleName,
			APIGroup: "rbac.authorization.k8s.io",
		},
	}

	_, err := fixture.KubeClientset.RbacV1().ClusterRoleBindings().Create(context.Background(), roleBinding, metav1.CreateOptions{})
	return err
}

func createServiceAccount(name, namespace string) error {
	serviceAccount := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	_, err := fixture.KubeClientset.CoreV1().ServiceAccounts(namespace).Create(context.Background(), serviceAccount, metav1.CreateOptions{})
	return err
}
