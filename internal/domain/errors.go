package domain

import (
	"errors"
	"fmt"
)

// ErrorClass is the stable classification attached to every operational failure.
// It is persisted and shown to administrators, so values must stay stable.
type ErrorClass string

const (
	ErrClassConfiguration  ErrorClass = "CONFIGURATION"
	ErrClassAuthentication ErrorClass = "AUTHENTICATION"
	ErrClassUnavailable    ErrorClass = "UNAVAILABLE"
	ErrClassTimeout        ErrorClass = "TIMEOUT"
	ErrClassCancellation   ErrorClass = "CANCELLATION"
	ErrClassCapacity       ErrorClass = "CAPACITY"
	ErrClassIntegrity      ErrorClass = "INTEGRITY"
	ErrClassExternalTool   ErrorClass = "EXTERNAL_TOOL"
	ErrClassInvariant      ErrorClass = "INVARIANT"
	ErrClassInternal       ErrorClass = "INTERNAL"
)

// Error is a typed operational error. Message must always be safe to persist,
// log, and render: callers are responsible for redacting secret material before
// constructing one.
type Error struct {
	Class     ErrorClass
	Code      string
	Message   string
	Retryable bool

	wrapped error
}

// NewError builds a typed error with a safe message.
func NewError(class ErrorClass, code, message string) *Error {
	return &Error{Class: class, Code: code, Message: message}
}

// WrapError builds a typed error that keeps an underlying cause for
// errors.Is/errors.As. The cause is never rendered into Message.
func WrapError(class ErrorClass, code, message string, cause error) *Error {
	return &Error{Class: class, Code: code, Message: message, wrapped: cause}
}

// Retry marks the error as retryable and returns it for chaining.
func (e *Error) Retry() *Error {
	e.Retryable = true
	return e
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s/%s: %s", e.Class, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.wrapped }

// SafeMessage returns the administrator-visible message.
func (e *Error) SafeMessage() string { return e.Message }

// ClassOf reports the class of err, defaulting to ErrClassInternal for errors
// that were not produced by this package.
func ClassOf(err error) ErrorClass {
	var typed *Error
	if AsError(err, &typed) {
		return typed.Class
	}
	return ErrClassInternal
}

// AsError extracts a typed error from an error chain. It is a thin wrapper over
// errors.As so that callers throughout the codebase do not each have to import
// errors just to read an error's class and safe message.
func AsError(err error, target **Error) bool {
	return errors.As(err, target)
}
