package badge

import (
	"fmt"
	"image/color"

	appv1 "github.com/argoproj/argo-cd/api/v3/pkg/apis/application/v1alpha1"
)

var (
	Blue   = color.RGBA{16, 61, 102, 255} // #103d66
	Green  = color.RGBA{11, 97, 42, 255}  // #0b612a
	Purple = color.RGBA{115, 31, 77, 255} // #731f4d
	Orange = color.RGBA{189, 115, 0, 255} // #bd7300
	Red    = color.RGBA{167, 46, 38, 255} // #a72e26
	Grey   = color.RGBA{41, 52, 61, 255}  // #29343D

	HealthStatusColors = map[appv1.HealthStatusCode]color.RGBA{
		appv1.HealthStatusDegraded:    Red,
		appv1.HealthStatusHealthy:     Green,
		appv1.HealthStatusMissing:     Purple,
		appv1.HealthStatusProgressing: Blue,
		appv1.HealthStatusSuspended:   Grey,
		appv1.HealthStatusUnknown:     Purple,
	}

	SyncStatusColors = map[appv1.SyncStatusCode]color.RGBA{
		appv1.SyncStatusCodeSynced:    Green,
		appv1.SyncStatusCodeOutOfSync: Orange,
		appv1.SyncStatusCodeUnknown:   Purple,
	}
)

func toRGBString(col color.RGBA) string {
	return fmt.Sprintf("rgb(%d, %d, %d)", col.R, col.G, col.B)
}
