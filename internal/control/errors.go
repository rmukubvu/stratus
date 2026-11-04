package control

import (
	"errors"
	"fmt"
)

// Kind classifies an Error so adapters can map it to a transport status
// without string matching.
type Kind string

const (
	KindInvalid     Kind = "invalid"
	KindNotFound    Kind = "not_found"
	KindConflict    Kind = "conflict"
	KindUnavailable Kind = "unavailable"
	KindInternal    Kind = "internal"
)

// Error is the control plane's domain error. Code is stable and machine
// readable, Hint tells a human or an agent what to do next.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Hint    string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// AsError returns err as a *Error, wrapping unknown errors as internal ones.
func AsError(err error) *Error {
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	return &Error{Kind: KindInternal, Code: "InternalError", Message: "control operation failed", Err: err}
}

func internalError(message string, err error) *Error {
	return &Error{Kind: KindInternal, Code: "InternalError", Message: message, Err: err}
}
