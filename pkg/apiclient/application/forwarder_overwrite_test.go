package application

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/test"
)

func TestProcessApplicationListField_SyncOperation(t *testing.T) {
	list := v1alpha1.ApplicationList{
		Items: []v1alpha1.Application{{Operation: &v1alpha1.Operation{Sync: &v1alpha1.SyncOperation{
			Revision: "abc",
		}}}},
	}

	res, err := processApplicationListField(&list, map[string]any{"items.operation.sync": true}, false)
	require.NoError(t, err)
	resMap, ok := res.(map[string]any)
	require.True(t, ok)

	items, ok := resMap["items"].([]map[string]any)
	require.True(t, ok)
	item := test.ToMap(items[0])

	val, ok, err := unstructured.NestedString(item, "operation", "sync", "revision")
	require.NoError(t, err)
	require.True(t, ok)

	require.Equal(t, "abc", val)
}

func TestProcessApplicationListField_PaginationMetadata(t *testing.T) {
	remaining := int64(42)
	list := v1alpha1.ApplicationList{
		ListMeta: metav1.ListMeta{
			Continue:           "app-next",
			RemainingItemCount: &remaining,
			ResourceVersion:    "12345",
		},
		Items: []v1alpha1.Application{{
			ObjectMeta: metav1.ObjectMeta{Name: "app-1"},
		}},
	}

	// Use a field set that includes items.metadata.name (common for list requests).
	res, err := processApplicationListField(&list, map[string]any{"items.metadata.name": true}, false)
	require.NoError(t, err)
	resMap, ok := res.(map[string]any)
	require.True(t, ok)

	// Verify ListMeta is preserved with pagination fields.
	meta, ok := resMap["metadata"]
	require.True(t, ok, "metadata should be present in response")
	metaMap, err := json.Marshal(meta)
	require.NoError(t, err)
	metaStr := string(metaMap)
	assert.Contains(t, metaStr, `"continue":"app-next"`)
	assert.Contains(t, metaStr, `"remainingItemCount":42`)
	assert.Contains(t, metaStr, `"resourceVersion":"12345"`)
}

func TestProcessApplicationListField_SyncOperationMissing(t *testing.T) {
	list := v1alpha1.ApplicationList{
		Items: []v1alpha1.Application{{Operation: nil}},
	}

	res, err := processApplicationListField(&list, map[string]any{"items.operation.sync": true}, false)
	require.NoError(t, err)
	resMap, ok := res.(map[string]any)
	require.True(t, ok)

	items, ok := resMap["items"].([]map[string]any)
	require.True(t, ok)
	item := test.ToMap(items[0])

	_, ok, err = unstructured.NestedString(item, "operation")
	require.NoError(t, err)
	require.False(t, ok)
}
