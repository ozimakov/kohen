package git

import (
	"errors"
	"fmt"
)

// Reason identifies why a git source operation failed, matching the Fetched
// condition reasons in SPEC §11.4 so the reconciler (S1.8) can surface it
// directly on the ConfigSync status.
type Reason string

const (
	// ReasonFetchFailed is a generic fetch/resolve failure (unreachable host,
	// ref not found, transport error).
	ReasonFetchFailed Reason = "FetchFailed"
	// ReasonAuthFailed indicates the credentials were rejected.
	ReasonAuthFailed Reason = "AuthFailed"
	// ReasonSourceNotAllowed indicates a URL rejected by the scheme allow-list,
	// SSRF guards, or the operator source allow-list (R-AUTH.3/.7).
	ReasonSourceNotAllowed Reason = "SourceNotAllowed"
	// ReasonPathNotFound indicates the requested subpath does not exist in the
	// resolved tree.
	ReasonPathNotFound Reason = "PathNotFound"
)

// Error is a git source failure carrying the SPEC condition reason and an
// optional wrapped cause.
type Error struct {
	Reason Reason
	Msg    string
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	return e.Msg
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *Error) Unwrap() error { return e.Err }

func newError(reason Reason, msg string) *Error {
	return &Error{Reason: reason, Msg: msg}
}

func wrapError(reason Reason, err error, format string, args ...any) *Error {
	return &Error{Reason: reason, Msg: fmt.Sprintf(format, args...), Err: err}
}

// ReasonOf extracts the git Reason from err, if any.
func ReasonOf(err error) (Reason, bool) {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Reason, true
	}
	return "", false
}
