package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/apply"
	"github.com/ozimakov/kohen/internal/git"
	"github.com/ozimakov/kohen/internal/render"
	"github.com/ozimakov/kohen/internal/wire"
)

// setCondition sets a status condition, stamping the object's generation.
func setCondition(cs *kohenv1alpha1.ConfigSync, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cs.Generation,
	})
}

// redactMsg strips any registered secret material from a message before it is
// written to status, events, or logs. R8.3/TM9 requires that no secret value
// ever appears in status or events, not only logs, so every dynamic message
// (error text, URLs) is funnelled through the redactor here.
func (r *ConfigSyncReconciler) redactMsg(msg string) string {
	if r.Redactor == nil {
		return msg
	}
	return r.Redactor.String(msg)
}

// loadCredential resolves the git credential from spec.source.authSecretRef.
// It enforces the required credential label (R-AUTH.6), registers secret
// material with the redactor (R8.3), and returns nil for anonymous access.
func (r *ConfigSyncReconciler) loadCredential(ctx context.Context, cs *kohenv1alpha1.ConfigSync) (*git.Credential, error) {
	ref := cs.Spec.Source.AuthSecretRef
	if ref == nil {
		return nil, nil
	}
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: cs.Namespace, Name: ref.Name}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("git credential secret %q not found", ref.Name)
		}
		return nil, fmt.Errorf("reading git credential secret: %w", err)
	}
	// R-AUTH.6: only Secrets explicitly labeled as git credentials are usable.
	if sec.Labels[kohenv1alpha1.LabelGitCredential] != "true" {
		return nil, fmt.Errorf("secret %q is missing the required label %s=true",
			ref.Name, kohenv1alpha1.LabelGitCredential)
	}

	cred := &git.Credential{
		Username:   string(sec.Data["username"]),
		Password:   string(sec.Data["password"]),
		PrivateKey: sec.Data["ssh-privatekey"],
		Passphrase: string(sec.Data["ssh-passphrase"]),
		KnownHosts: sec.Data["known_hosts"],
	}
	if cred.Password == "" {
		cred.Password = string(sec.Data["token"])
	}

	// Insecure transport is an explicit, logged opt-in that the operator must
	// also permit at install time (SPEC R7.8, R-AUTH.7).
	if r.Config != nil && r.Config.AllowInsecureGitTLS {
		log := logf.FromContext(ctx)
		if string(sec.Data["insecure-skip-tls-verify"]) == "true" {
			cred.InsecureSkipTLSVerify = true
			log.Info("insecure git TLS verification enabled for source", "secret", ref.Name)
		}
		if string(sec.Data["insecure-ignore-host-key"]) == "true" {
			cred.InsecureIgnoreHostKey = true
			log.Info("insecure SSH host-key verification enabled for source", "secret", ref.Name)
		}
	}

	if r.Redactor != nil {
		r.Redactor.Add(cred.Password, cred.Passphrase, string(cred.PrivateKey))
	}
	return cred, nil
}

// buildConfigMap assembles the target ConfigMap from rendered content. Ownership
// labels and the owner reference are added by the apply engine.
func buildConfigMap(cs *kohenv1alpha1.ConfigSync, name string, rendered *render.Result) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cs.Namespace},
	}
	if len(rendered.Data) > 0 {
		cm.Data = rendered.Data
	}
	if len(rendered.BinaryData) > 0 {
		cm.BinaryData = rendered.BinaryData
	}
	return cm
}

// gitConditionReason maps a git error to a Fetched condition reason (§11.4).
func gitConditionReason(err error) string {
	if reason, ok := git.ReasonOf(err); ok {
		return string(reason)
	}
	return kohenv1alpha1.ReasonFetchFailed
}

// renderConditionReason maps a render error to a Rendered condition reason.
func renderConditionReason(err error) string {
	if reason, ok := render.ReasonOf(err); ok {
		return string(reason)
	}
	return kohenv1alpha1.ReasonDegraded
}

// wireConditionReason maps a wiring error to a WorkloadWired condition reason.
func wireConditionReason(err error) string {
	if reason, ok := wire.ReasonOf(err); ok {
		return string(reason)
	}
	return kohenv1alpha1.ReasonDegraded
}

// applyConditionReason maps an apply error to a reason string.
func applyConditionReason(err error) string {
	if reason, ok := apply.ReasonOf(err); ok {
		return string(reason)
	}
	return kohenv1alpha1.ReasonDegraded
}
