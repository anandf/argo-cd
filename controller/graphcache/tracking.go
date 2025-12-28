package graphcache

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// Argo CD tracking label and annotation keys
	// From common/common.go
	LabelKeyAppInstance      = "app.kubernetes.io/instance"
	AnnotationKeyAppInstance = "argocd.argoproj.io/tracking-id"
)

// TrackingMethod defines how resources are tracked.
type TrackingMethod string

const (
	TrackingMethodLabel            TrackingMethod = "label"
	TrackingMethodAnnotation       TrackingMethod = "annotation"
	TrackingMethodAnnotationAndLabel TrackingMethod = "annotation+label"
)

// TrackingInfo contains information extracted from Argo CD tracking labels/annotations.
type TrackingInfo struct {
	HasTracking bool   // Whether the resource has Argo CD tracking
	AppName     string // Application name
	TrackingID  string // Full tracking ID
	Method      TrackingMethod // How it was tracked
}

// ExtractTrackingInfo extracts Argo CD tracking information from a resource.
// It supports both label-based and annotation-based tracking methods.
func ExtractTrackingInfo(obj *unstructured.Unstructured, method TrackingMethod) TrackingInfo {
	info := TrackingInfo{
		HasTracking: false,
	}

	if obj == nil {
		return info
	}

	switch method {
	case TrackingMethodLabel:
		info = extractFromLabel(obj)
	case TrackingMethodAnnotation:
		info = extractFromAnnotation(obj)
	case TrackingMethodAnnotationAndLabel:
		// Try annotation first (more specific), fall back to label
		info = extractFromAnnotation(obj)
		if !info.HasTracking {
			info = extractFromLabel(obj)
		}
	}

	return info
}

// extractFromLabel extracts tracking info from the instance label.
func extractFromLabel(obj *unstructured.Unstructured) TrackingInfo {
	info := TrackingInfo{}

	labels := obj.GetLabels()
	if labels == nil {
		return info
	}

	// Check for app.kubernetes.io/instance label (legacy tracking)
	appName, exists := labels[LabelKeyAppInstance]
	if exists && appName != "" {
		info.HasTracking = true
		info.AppName = appName
		info.Method = TrackingMethodLabel
		// Generate tracking ID from resource info
		info.TrackingID = generateTrackingID(appName, obj)
	}

	return info
}

// extractFromAnnotation extracts tracking info from the tracking annotation.
func extractFromAnnotation(obj *unstructured.Unstructured) TrackingInfo {
	info := TrackingInfo{}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		return info
	}

	// Check for tracking annotation
	trackingID, exists := annotations[AnnotationKeyAppInstance]
	if exists && trackingID != "" {
		info.HasTracking = true
		info.TrackingID = trackingID
		info.AppName = extractAppNameFromTrackingID(trackingID)
		info.Method = TrackingMethodAnnotation
	}

	return info
}

// extractAppNameFromTrackingID extracts the application name from a tracking ID.
// Tracking ID format: <app-name>:<group>/<kind>:<namespace>/<name>
func extractAppNameFromTrackingID(trackingID string) string {
	parts := strings.SplitN(trackingID, ":", 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// generateTrackingID generates a tracking ID from resource information.
// Format: <app-name>:<group>/<kind>:<namespace>/<name>
func generateTrackingID(appName string, obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()

	group := gvk.Group
	if group == "" {
		group = "core"
	}

	namespace := obj.GetNamespace()
	name := obj.GetName()

	if namespace != "" {
		return fmt.Sprintf("%s:%s/%s:%s/%s", appName, group, gvk.Kind, namespace, name)
	}

	// Cluster-scoped resource
	return fmt.Sprintf("%s:%s/%s:%s", appName, group, gvk.Kind, name)
}

// HasArgoTracking is a convenience function to check if a resource has any Argo CD tracking.
func HasArgoTracking(obj *unstructured.Unstructured, method TrackingMethod) bool {
	info := ExtractTrackingInfo(obj, method)
	return info.HasTracking
}

// GetManagedByApp returns the application name managing this resource, or empty string if not managed.
func GetManagedByApp(obj *unstructured.Unstructured, method TrackingMethod) string {
	info := ExtractTrackingInfo(obj, method)
	if info.HasTracking {
		return info.AppName
	}
	return ""
}

// BuildLabelSelector builds a label selector for finding resources managed by Argo CD.
// This is used for initial discovery queries.
func BuildLabelSelector() string {
	// Select resources with the app.kubernetes.io/instance label
	return fmt.Sprintf("%s", LabelKeyAppInstance)
}

// ParseTrackingMethod converts a string to TrackingMethod.
func ParseTrackingMethod(s string) (TrackingMethod, error) {
	switch strings.ToLower(s) {
	case "label":
		return TrackingMethodLabel, nil
	case "annotation":
		return TrackingMethodAnnotation, nil
	case "annotation+label":
		return TrackingMethodAnnotationAndLabel, nil
	default:
		return "", fmt.Errorf("invalid tracking method: %s (valid: label, annotation, annotation+label)", s)
	}
}

// String returns the string representation of TrackingMethod.
func (t TrackingMethod) String() string {
	return string(t)
}

// DefaultTrackingMethod returns the default tracking method.
func DefaultTrackingMethod() TrackingMethod {
	// Use annotation+label for maximum compatibility
	return TrackingMethodAnnotationAndLabel
}
