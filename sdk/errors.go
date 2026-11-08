package sdk

import (
	"errors"
	"net"
	"syscall"
)

// ErrorType says whether retrying can help.
type ErrorType string

const (
	ErrorTypeTransient ErrorType = "transient"
	ErrorTypePermanent ErrorType = "permanent"
)

// ConduitError is the error HTTPClient and connectors return; the gateway labels
// error metrics with Type.
type ConduitError struct {
	Type    ErrorType
	Message string
	Code    int // HTTP status code, or 0 if the error did not come from a response
	Err     error
}

func (e *ConduitError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *ConduitError) Unwrap() error {
	return e.Err
}

// NewTransientError returns a retryable error.
func NewTransientError(msg string, code int, err error) error {
	return &ConduitError{Type: ErrorTypeTransient, Message: msg, Code: code, Err: err}
}

// NewPermanentError returns an error that retrying cannot fix.
func NewPermanentError(msg string, code int, err error) error {
	return &ConduitError{Type: ErrorTypePermanent, Message: msg, Code: code, Err: err}
}

// IsTransient reports whether err is worth retrying: a transient ConduitError,
// a timeout, or a dropped connection.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var ce *ConduitError
	if errors.As(err, &ce) {
		return ce.Type == ErrorTypeTransient
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE)
}

// IsPermanent reports whether err is non-nil and not transient.
func IsPermanent(err error) bool {
	if err == nil {
		return false
	}
	return !IsTransient(err)
}
