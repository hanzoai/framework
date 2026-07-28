package framework

import (
	"errors"

	"github.com/hanzoai/doctype"
)

// errors.go is the engine's failure vocabulary.
//
// The engine classifies WHAT WENT WRONG; a host maps that to its own status
// vocabulary (Hanzo Cloud maps it to HTTP). Keeping the classification here is
// deliberate: "a Link points at a record that does not exist in this org" is
// engine knowledge, and if each host re-derived it from error strings they
// would drift. Hosts call Classify and switch on a Code — never on a message.

// Sentinel errors. Compare with errors.Is.
var (
	ErrNotFound  = errors.New("framework: not found")
	ErrConflict  = errors.New("framework: already exists")
	ErrBadRef    = errors.New("framework: referenced record not found in org")
	ErrBadState  = errors.New("framework: illegal docstatus transition")
	ErrForbidden = errors.New("framework: forbidden")
)

// HookAbort wraps the error a GATE hook returned. A gate hook
// (before_insert / before_save / on_submit / on_cancel / on_trash) that returns
// an error aborts the operation before any state change; wrapping it preserves
// the hook's message while telling the host this was a refusal, not a fault.
type HookAbort struct{ Err error }

func (h *HookAbort) Error() string { return h.Err.Error() }
func (h *HookAbort) Unwrap() error { return h.Err }

// Code is what an engine failure MEANS.
type Code int

const (
	// CodeInternal is an unexpected fault — the store failed, a marshal failed.
	CodeInternal Code = iota
	// CodeInvalid is a malformed request: a schema violation, a bad field value,
	// an unknown filter field. The caller must change what they sent.
	CodeInvalid
	// CodeForbidden is an authorization refusal: no validated tenant, or a right
	// the caller's roles do not carry.
	CodeForbidden
	// CodeNotFound is a DocType or document that does not exist in this org.
	CodeNotFound
	// CodeConflict is a uniqueness or lifecycle-state collision: the record
	// already exists, or the document is not in a state that permits this.
	CodeConflict
	// CodeRejected is a well-formed request the engine refused on the data's
	// own terms: a dangling Link, or a gate hook that vetoed the operation.
	CodeRejected
)

// Classify maps any engine error to its Code. This is the ONE place that
// decides what a failure means; a host switches on the result and never
// re-derives the classification.
func Classify(err error) Code {
	var abort *HookAbort
	switch {
	case err == nil:
		return CodeInternal // callers must check err != nil first
	case errors.Is(err, ErrForbidden):
		return CodeForbidden
	case errors.Is(err, ErrNotFound):
		return CodeNotFound
	case errors.Is(err, ErrConflict):
		return CodeConflict
	case errors.Is(err, ErrBadState):
		return CodeConflict
	case errors.Is(err, ErrBadRef):
		return CodeRejected
	case errors.As(err, &abort):
		return CodeRejected
	case doctype.IsValidationError(err):
		return CodeInvalid
	default:
		return CodeInternal
	}
}

// IsValidationError reports whether err is a document-schema violation, so an
// in-process caller can answer "bad request" without string-matching. It is the
// value layer's predicate, re-exported so a caller that holds only the engine
// import does not need a second one.
func IsValidationError(err error) bool { return doctype.IsValidationError(err) }
