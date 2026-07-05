package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolloutMode controls whether a config change triggers a workload rollout.
// +kubebuilder:validation:Enum=auto;none
type RolloutMode string

const (
	// RolloutAuto rolls the workload when the config version changes (default).
	RolloutAuto RolloutMode = "auto"
	// RolloutNone updates objects but never triggers a rollout.
	RolloutNone RolloutMode = "none"
)

// SecretBackend selects how a referenced secret is resolved. In v1 only ESO and
// native Secrets are supported (SPEC §8.3).
// +kubebuilder:validation:Enum=externalSecret;nativeSecret
type SecretBackend string

const (
	// BackendExternalSecret resolves via an External Secrets Operator object.
	BackendExternalSecret SecretBackend = "externalSecret"
	// BackendNativeSecret resolves a pre-existing native Secret.
	BackendNativeSecret SecretBackend = "nativeSecret"
)

// LocalObjectReference names another object in the same namespace as the
// ConfigSync. Namespace locality is enforced by construction — there is no
// namespace field (SPEC R-AUTH.5).
type LocalObjectReference struct {
	// Name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// GitSource identifies the git repository, ref, and optional credentials.
type GitSource struct {
	// URL is the repository URL over HTTPS or SSH (SPEC R7.1).
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// Ref is a branch, tag, or commit SHA. Branch tracking follows the moving
	// branch; tag/commit pins are immutable (SPEC R7.2).
	// +kubebuilder:default=main
	// +optional
	Ref string `json:"ref,omitempty"`

	// AuthSecretRef references a Secret (same namespace) holding git credentials.
	// The Secret MUST be labeled kohen.dev/git-credential=true (SPEC R7.8,
	// R-AUTH.6), enforced at reconcile time.
	// +optional
	AuthSecretRef *LocalObjectReference `json:"authSecretRef,omitempty"`
}

// WorkloadReference identifies the target workload in the ConfigSync namespace.
type WorkloadReference struct {
	// Kind of the workload. Only RollingUpdate-capable kinds are supported.
	// +kubebuilder:validation:Enum=Deployment;StatefulSet
	Kind string `json:"kind"`

	// Name of the workload.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ConfigMapSpec configures the target ConfigMap.
type ConfigMapSpec struct {
	// Name of the ConfigMap. Defaults to "<workloadRef.name>-config" (SPEC
	// §11.2); the default is applied at reconcile time (see Spec.ConfigMapName).
	// +optional
	Name string `json:"name,omitempty"`
}

// Wiring configures how the rendered config is mounted into the workload.
type Wiring struct {
	// Container to wire. Defaults to the first container of the pod spec (SPEC
	// §11.2), applied at reconcile time.
	// +optional
	Container string `json:"container,omitempty"`

	// MountPath is the volume mount path for the config.
	// +kubebuilder:default=/etc/kohen/config
	// +kubebuilder:validation:MaxLength=253
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}

// SyncSpec configures reconcile timing.
type SyncSpec struct {
	// Interval between polling reconciles.
	// +kubebuilder:default="30s"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`
}

// SecretSurfaceMode selects how a resolved secret is surfaced into the pod.
// +kubebuilder:validation:Enum=file;env
type SecretSurfaceMode string

const (
	// SurfaceFile mounts the secret as files.
	SurfaceFile SecretSurfaceMode = "file"
	// SurfaceEnv injects the secret as environment variables.
	SurfaceEnv SecretSurfaceMode = "env"
)

// SecretSurface configures how a resolved secret is wired into the pod (SPEC
// §8.4). File surfacing mounts the backing Secret as a volume; env surfacing
// injects a single key as a discrete env entry with valueFrom.secretKeyRef
// (no envFrom — R-WIRE.2).
//
// The `as` field is a CEL-reserved keyword and cannot be referenced by an
// OpenAPI validation rule, so CEL enforces the mode-specific field set by
// presence (a surface carries EITHER mountPath, for file, XOR envVar+key, for
// env); the reconciler additionally verifies that `as` matches the field set
// (SPEC R11.1) and fails the reference closed if not.
//
// +kubebuilder:validation:XValidation:rule="(has(self.mountPath) && !has(self.envVar) && !has(self.key)) || (!has(self.mountPath) && has(self.envVar) && has(self.key))",message="surface must set mountPath (as=file) or both envVar and key (as=env), and only those fields"
type SecretSurface struct {
	// As selects file or env surfacing.
	As SecretSurfaceMode `json:"as"`

	// MountPath is the mount path for the secret volume (as=file). Kohen never
	// uses subPath, so kubelet delivers rotations in place (R8.5).
	// +kubebuilder:validation:MaxLength=253
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// EnvVar is the environment variable name to inject (as=env).
	// +kubebuilder:validation:MaxLength=253
	// +optional
	EnvVar string `json:"envVar,omitempty"`

	// Key is the Secret data key exposed via the env var (as=env).
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Key string `json:"key,omitempty"`

	// RolloutOnRotate controls whether an env-surfaced rotation advances the
	// config version and rolls the workload (SPEC R8.5). Defaults to true.
	// File-surfaced references ignore this (kubelet delivers rotations in
	// place, never a rollout).
	// +kubebuilder:default=true
	// +optional
	RolloutOnRotate *bool `json:"rolloutOnRotate,omitempty"`
}

// ShouldRolloutOnRotate reports the effective rolloutOnRotate value, applying
// the true default when unset (SPEC §11.2).
func (s *SecretSurface) ShouldRolloutOnRotate() bool {
	if s.RolloutOnRotate == nil {
		return true
	}
	return *s.RolloutOnRotate
}

// SecretReference declares a secret the config references and how it is
// surfaced into the workload (SPEC §8.1, §8.4).
//
// +kubebuilder:validation:XValidation:rule="self.backend != 'externalSecret' || (has(self.externalSecret) && !has(self.nativeSecret))",message="backend externalSecret requires the externalSecret ref and forbids nativeSecret"
// +kubebuilder:validation:XValidation:rule="self.backend != 'nativeSecret' || (has(self.nativeSecret) && !has(self.externalSecret))",message="backend nativeSecret requires the nativeSecret ref and forbids externalSecret"
type SecretReference struct {
	// Name is the unique local name of this reference. It must be a DNS-1123
	// label so it can form a deterministic, valid volume name when surfaced as
	// a file.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=50
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Backend selects the resolution mechanism.
	Backend SecretBackend `json:"backend"`

	// ExternalSecret references the ExternalSecret object (backend=externalSecret).
	// +optional
	ExternalSecret *LocalObjectReference `json:"externalSecret,omitempty"`

	// NativeSecret references a native Secret (backend=nativeSecret).
	// +optional
	NativeSecret *LocalObjectReference `json:"nativeSecret,omitempty"`

	// Surface configures how the resolved secret is wired into the pod.
	Surface SecretSurface `json:"surface"`
}

// SecretName returns the name of the backing Secret/object this reference points
// at (the ExternalSecret name for the ESO backend — whose target Secret shares
// the name by default — or the native Secret name).
func (r *SecretReference) SecretName() string {
	switch r.Backend {
	case BackendExternalSecret:
		if r.ExternalSecret != nil {
			return r.ExternalSecret.Name
		}
	case BackendNativeSecret:
		if r.NativeSecret != nil {
			return r.NativeSecret.Name
		}
	}
	return ""
}

// ConfigSyncSpec defines the desired state of a ConfigSync (SPEC §11.1).
// +kubebuilder:validation:XValidation:rule="!self.path.startsWith('/') && !self.path.contains('..')",message="path must be a repository-relative path without '..' segments"
// The env/file distinction below is expressed through the presence of
// surface.envVar / surface.mountPath (a CEL-safe proxy for surface.as; see
// SecretSurface) so the rules can compile without naming the reserved `as`
// field.
// +kubebuilder:validation:XValidation:rule="self.rollout != 'none' || !has(self.secretRefs) || self.secretRefs.all(r, !has(r.surface.envVar))",message="env-surfaced secretRefs require a rollout; remove rollout: none or use surface.as=file (SPEC §9.1)"
// +kubebuilder:validation:XValidation:rule="!has(self.secretRefs) || self.secretRefs.filter(r, has(r.surface.mountPath)).all(r1, self.secretRefs.filter(r2, has(r2.surface.mountPath) && r2.surface.mountPath == r1.surface.mountPath).size() == 1)",message="secretRefs file mountPaths must be unique (R8.12)"
// +kubebuilder:validation:XValidation:rule="!has(self.secretRefs) || self.secretRefs.filter(r, has(r.surface.envVar)).all(r1, self.secretRefs.filter(r2, has(r2.surface.envVar) && r2.surface.envVar == r1.surface.envVar).size() == 1)",message="secretRefs env var names must be unique (R8.12)"
// +kubebuilder:validation:XValidation:rule="!has(self.secretRefs) || self.secretRefs.filter(r, has(r.surface.mountPath)).all(r, r.surface.mountPath != self.wiring.mountPath)",message="a secretRef file mountPath must not equal wiring.mountPath (R8.12)"
type ConfigSyncSpec struct {
	// Source is the git repository, ref, and optional credentials.
	Source GitSource `json:"source"`

	// Path is the repository-relative path whose files are rendered.
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// WorkloadRef is the target workload (same namespace).
	WorkloadRef WorkloadReference `json:"workloadRef"`

	// ConfigMap configures the target ConfigMap.
	// +optional
	ConfigMap ConfigMapSpec `json:"configMap,omitempty"`

	// Wiring configures how config is mounted into the workload.
	// +kubebuilder:default={mountPath:"/etc/kohen/config"}
	// +optional
	Wiring Wiring `json:"wiring,omitempty"`

	// Rollout controls whether config changes trigger a rollout.
	// +kubebuilder:default=auto
	// +optional
	Rollout RolloutMode `json:"rollout,omitempty"`

	// Sync configures reconcile timing.
	// +kubebuilder:default={interval:"30s"}
	// +optional
	Sync SyncSpec `json:"sync,omitempty"`

	// SecretRefs declares secrets the config references and how they surface
	// into the workload (SPEC §8). Bounded to keep CEL validation cost within
	// the API server budget; one ConfigSync per workload rarely needs many.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	SecretRefs []SecretReference `json:"secretRefs,omitempty"`
}

// SecretRefStatus reports per-reference resolution state — never values (SPEC
// §11.1, R11.2).
type SecretRefStatus struct {
	// Name of the reference.
	Name string `json:"name"`
	// Resolved indicates whether the reference is currently resolved.
	Resolved bool `json:"resolved"`
	// Backend is the reference's backend.
	// +optional
	Backend SecretBackend `json:"backend,omitempty"`
	// Reason explains a non-resolved state.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Established is a sticky marker: once a reference has been resolved as part
	// of an applied config version, it stays true. It drives the asymmetric
	// readiness policy (R8.9) — an established reference that becomes
	// transiently not-ready fails safe (keep last-good) rather than fail closed.
	// +optional
	Established bool `json:"established,omitempty"`
}

// ConfigSyncStatus is the observed state of a ConfigSync (SPEC §11.1, §11.4).
type ConfigSyncStatus struct {
	// ObservedGeneration is the last spec generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SourceCommit is the resolved git commit SHA (plain SHA, correlates with
	// git log).
	// +optional
	SourceCommit string `json:"sourceCommit,omitempty"`

	// ConfigVersion is the desired rollout-trigger identity (stamped value).
	// +optional
	ConfigVersion string `json:"configVersion,omitempty"`

	// WorkloadVersion is the version currently stamped on the workload.
	// +optional
	WorkloadVersion string `json:"workloadVersion,omitempty"`

	// RolloutInProgress reflects whether the workload is mid-rollout.
	// +optional
	RolloutInProgress bool `json:"rolloutInProgress,omitempty"`

	// SecretRefs is the per-reference resolution state (no values).
	// +optional
	// +listType=map
	// +listMapKey=name
	SecretRefs []SecretRefStatus `json:"secretRefs,omitempty"`

	// Conditions is the standard condition set (SPEC §11.4).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ConfigSync keeps a target workload in sync with a path in a git config repo.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cs,categories=kohen
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Source-Commit",type=string,JSONPath=`.status.sourceCommit`
// +kubebuilder:printcolumn:name="Config-Version",type=string,JSONPath=`.status.configVersion`
// +kubebuilder:printcolumn:name="Workload-Version",type=string,JSONPath=`.status.workloadVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ConfigSync struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConfigSyncSpec   `json:"spec"`
	Status ConfigSyncStatus `json:"status,omitempty"`
}

// ConfigSyncList is a list of ConfigSync resources.
//
// +kubebuilder:object:root=true
type ConfigSyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConfigSync `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ConfigSync{}, &ConfigSyncList{})
}

// DefaultSyncInterval is the reconcile poll interval when unset (SPEC §11.2).
const DefaultSyncInterval = 30 * time.Second

// ConfigMapName returns the effective target ConfigMap name, applying the
// default of "<workloadRef.name>-config" (SPEC §11.2).
func (s *ConfigSyncSpec) ConfigMapName() string {
	if s.ConfigMap.Name != "" {
		return s.ConfigMap.Name
	}
	return s.WorkloadRef.Name + "-config"
}

// SyncInterval returns the effective reconcile interval, applying the 30s
// default (SPEC §11.2). This is robust regardless of how the object was created,
// since metav1.Duration cannot be omitted via omitempty and may arrive as zero.
func (s *ConfigSyncSpec) SyncInterval() time.Duration {
	if s.Sync.Interval.Duration <= 0 {
		return DefaultSyncInterval
	}
	return s.Sync.Interval.Duration
}
