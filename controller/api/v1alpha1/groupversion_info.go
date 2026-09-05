// Package v1alpha1 contains the Go types for the devplatform.fickledev.com/v1alpha1
// CRDs. The CRD YAML under apps/devplatform/templates/crd-*.yaml is the source of
// truth for validation rules; these types must stay field-compatible with it.
// +kubebuilder:object:generate=true
// +groupName=devplatform.fickledev.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "devplatform.fickledev.com", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
