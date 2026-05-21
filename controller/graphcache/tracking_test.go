package graphcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func makeTestObj(group, version, kind, namespace, name string, labels, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": func() string {
				if group == "" {
					return version
				}
				return group + "/" + version
			}(),
			"kind": kind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
		},
	}
	if labels != nil {
		obj.SetLabels(labels)
	}
	if annotations != nil {
		obj.SetAnnotations(annotations)
	}
	return obj
}

func TestExtractTrackingInfo_Label(t *testing.T) {
	obj := makeTestObj("apps", "v1", "Deployment", "default", "nginx",
		map[string]string{LabelKeyAppInstance: "myapp"}, nil)

	info := ExtractTrackingInfo(obj, TrackingMethodLabel)
	assert.True(t, info.HasTracking)
	assert.Equal(t, "myapp", info.AppName)
	assert.Equal(t, TrackingMethodLabel, info.Method)
	assert.Contains(t, info.TrackingID, "myapp:")
}

func TestExtractTrackingInfo_Annotation(t *testing.T) {
	obj := makeTestObj("apps", "v1", "Deployment", "default", "nginx",
		nil, map[string]string{AnnotationKeyAppInstance: "myapp:apps/Deployment:default/nginx"})

	info := ExtractTrackingInfo(obj, TrackingMethodAnnotation)
	assert.True(t, info.HasTracking)
	assert.Equal(t, "myapp", info.AppName)
	assert.Equal(t, TrackingMethodAnnotation, info.Method)
	assert.Equal(t, "myapp:apps/Deployment:default/nginx", info.TrackingID)
}

func TestExtractTrackingInfo_AnnotationAndLabel_PrefersAnnotation(t *testing.T) {
	obj := makeTestObj("apps", "v1", "Deployment", "default", "nginx",
		map[string]string{LabelKeyAppInstance: "label-app"},
		map[string]string{AnnotationKeyAppInstance: "annotation-app:apps/Deployment:default/nginx"})

	info := ExtractTrackingInfo(obj, TrackingMethodAnnotationAndLabel)
	assert.True(t, info.HasTracking)
	assert.Equal(t, "annotation-app", info.AppName)
	assert.Equal(t, TrackingMethodAnnotation, info.Method)
}

func TestExtractTrackingInfo_AnnotationAndLabel_FallsBackToLabel(t *testing.T) {
	obj := makeTestObj("apps", "v1", "Deployment", "default", "nginx",
		map[string]string{LabelKeyAppInstance: "myapp"}, nil)

	info := ExtractTrackingInfo(obj, TrackingMethodAnnotationAndLabel)
	assert.True(t, info.HasTracking)
	assert.Equal(t, "myapp", info.AppName)
	assert.Equal(t, TrackingMethodLabel, info.Method)
}

func TestExtractTrackingInfo_NoTracking(t *testing.T) {
	obj := makeTestObj("apps", "v1", "Deployment", "default", "nginx", nil, nil)

	info := ExtractTrackingInfo(obj, TrackingMethodLabel)
	assert.False(t, info.HasTracking)
	assert.Empty(t, info.AppName)
}

func TestExtractTrackingInfo_NilObj(t *testing.T) {
	info := ExtractTrackingInfo(nil, TrackingMethodLabel)
	assert.False(t, info.HasTracking)
}

func TestHasArgoTracking(t *testing.T) {
	tracked := makeTestObj("", "v1", "Pod", "default", "nginx",
		map[string]string{LabelKeyAppInstance: "myapp"}, nil)
	untracked := makeTestObj("", "v1", "Pod", "default", "nginx", nil, nil)

	assert.True(t, HasArgoTracking(tracked, TrackingMethodLabel))
	assert.False(t, HasArgoTracking(untracked, TrackingMethodLabel))
}

func TestGetManagedByApp(t *testing.T) {
	obj := makeTestObj("", "v1", "Pod", "default", "nginx",
		map[string]string{LabelKeyAppInstance: "myapp"}, nil)

	assert.Equal(t, "myapp", GetManagedByApp(obj, TrackingMethodLabel))
	assert.Empty(t, GetManagedByApp(makeTestObj("", "v1", "Pod", "default", "x", nil, nil), TrackingMethodLabel))
}

func TestParseTrackingMethod(t *testing.T) {
	tests := []struct {
		input   string
		want    TrackingMethod
		wantErr bool
	}{
		{"label", TrackingMethodLabel, false},
		{"annotation", TrackingMethodAnnotation, false},
		{"annotation+label", TrackingMethodAnnotationAndLabel, false},
		{"LABEL", TrackingMethodLabel, false},
		{"invalid", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseTrackingMethod(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestExtractAppNameFromTrackingID(t *testing.T) {
	assert.Equal(t, "myapp", extractAppNameFromTrackingID("myapp:apps/Deployment:default/nginx"))
	assert.Equal(t, "myapp", extractAppNameFromTrackingID("myapp:"))
	assert.Equal(t, "myapp", extractAppNameFromTrackingID("myapp"))
}

func TestGenerateTrackingID(t *testing.T) {
	// Namespaced resource
	obj := makeTestObj("apps", "v1", "Deployment", "default", "nginx", nil, nil)
	id := generateTrackingID("myapp", obj)
	assert.Equal(t, "myapp:apps/Deployment:default/nginx", id)

	// Core group namespaced
	coreObj := makeTestObj("", "v1", "Pod", "default", "nginx", nil, nil)
	coreID := generateTrackingID("myapp", coreObj)
	assert.Equal(t, "myapp:/Pod:default/nginx", coreID)

	// Cluster-scoped
	clusterObj := makeTestObj("", "v1", "Namespace", "", "kube-system", nil, nil)
	clusterID := generateTrackingID("myapp", clusterObj)
	assert.Equal(t, "myapp:/Namespace:kube-system", clusterID)
}

func TestDefaultTrackingMethod(t *testing.T) {
	assert.Equal(t, TrackingMethodAnnotationAndLabel, DefaultTrackingMethod())
}

func TestBuildLabelSelector(t *testing.T) {
	selector := BuildLabelSelector()
	assert.Equal(t, LabelKeyAppInstance, selector)
}
