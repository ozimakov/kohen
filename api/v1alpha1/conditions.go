package v1alpha1

// Condition types for ConfigSync status (SPEC §11.4).
const (
	// ConditionReady is the overall condition: desired version fully applied,
	// wired, and converged.
	ConditionReady = "Ready"
	// ConditionFetched reports git fetch/resolve of spec.source.
	ConditionFetched = "Fetched"
	// ConditionRendered reports that config rendered within limits.
	ConditionRendered = "Rendered"
	// ConditionSecretsReady reports that all references resolved (R8.9).
	ConditionSecretsReady = "SecretsReady"
	// ConditionWorkloadWired reports SSA merge of owned fields.
	ConditionWorkloadWired = "WorkloadWired"
	// ConditionRolloutComplete reports the stamped version rolled out.
	ConditionRolloutComplete = "RolloutComplete"
)

// Condition reasons (SPEC §11.4). Every §10 failure row maps to one of these
// (R10.2).
const (
	// Ready reasons.
	ReasonSynced      = "Synced"
	ReasonProgressing = "Progressing"
	ReasonDegraded    = "Degraded"

	// Fetched reasons.
	ReasonFetchFailed      = "FetchFailed"
	ReasonAuthFailed       = "AuthFailed"
	ReasonSourceNotAllowed = "SourceNotAllowed"
	ReasonPathNotFound     = "PathNotFound"

	// Rendered reasons.
	ReasonOversize            = "Oversize"
	ReasonTreeSafetyViolation = "TreeSafetyViolation"
	ReasonInvalidKey          = "InvalidKey"
	ReasonKeyConflict         = "KeyConflict"

	// SecretsReady reasons (Phase 2). §11.4 lists these as example reasons;
	// InvalidSurface is a Kohen extension for a surface whose declared `as`
	// mode does not match its fields (CEL cannot reference the reserved `as`
	// field, so the reconciler validates it — R11.1).
	ReasonSecretNotFound          = "SecretNotFound"
	ReasonKeyMissing              = "KeyMissing"
	ReasonAwaitingFirstResolution = "AwaitingFirstResolution"
	ReasonBackendNotReady         = "BackendNotReady"
	ReasonDegradedServingLastGood = "DegradedServingLastGood"
	ReasonMaxDegradedExceeded     = "MaxDegradedExceeded"
	ReasonInvalidSurface          = "InvalidSurface"

	// WorkloadWired reasons.
	ReasonWorkloadNotFound    = "WorkloadNotFound"
	ReasonUnsupportedStrategy = "UnsupportedStrategy"
	ReasonApplyConflict       = "ApplyConflict"
	ReasonSingletonViolation  = "SingletonViolation"

	// RolloutComplete reasons.
	ReasonRollingOut               = "RollingOut"
	ReasonProgressDeadlineExceeded = "ProgressDeadlineExceeded"
)

// Well-known annotations and labels (SPEC §11.2, §6.1, R8.8).
const (
	// AnnotationConfigSHA is the fixed version-stamp annotation on the pod
	// template (SPEC §11.2).
	AnnotationConfigSHA = "kohen.dev/config-sha"
	// AnnotationSyncNow forces an immediate reconcile (SPEC §6.1).
	AnnotationSyncNow = "kohen.dev/sync-now"
	// LabelGitCredential marks a Secret usable as git credentials (R-AUTH.6).
	LabelGitCredential = "kohen.dev/git-credential"
)
