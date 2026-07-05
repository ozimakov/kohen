// Package v1alpha1 contains the Kohen API types for group kohen.dev.
//
// The primary (and, in v1, only) control surface is the ConfigSync resource
// (SPEC §11.1, §7.3). See SPEC §11 for the full API contract and §11.4 for the
// status conditions this package declares as typed constants.
//
// +kubebuilder:object:generate=true
// +groupName=kohen.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the API group and version for Kohen types.
var GroupVersion = schema.GroupVersion{Group: "kohen.dev", Version: "v1alpha1"}

// SchemeBuilder registers the Kohen types with a runtime scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the Kohen types to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme
