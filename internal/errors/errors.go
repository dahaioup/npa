package errors

import (
	"errors"
	"fmt"
)

type Code string

const (
	InvalidArgument  Code = "invalid_argument"
	PermissionDenied Code = "permission_denied"
	Authentication   Code = "authentication_failed"
	HostKeyChanged   Code = "host_key_changed"
	NodeOffline      Code = "node_offline"
	Timeout          Code = "timeout"
	RateLimited      Code = "rate_limited"
	PortInUse        Code = "port_in_use"
	TargetForbidden  Code = "target_forbidden"
	NotSupported     Code = "not_supported"
	Internal         Code = "internal_error"
)

type AppError struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}

func (e *AppError) Error() string { return e.Message }
func (e *AppError) Unwrap() error { return e.Cause }

func New(code Code, message string) error {
	return &AppError{Code: code, Message: message}
}

func Wrap(code Code, message string, cause error) error {
	return &AppError{Code: code, Message: fmt.Sprintf("%s: %v", message, cause), Cause: cause}
}

func CodeOf(err error) Code {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return Internal
}

func MessageOf(err error) string {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr.Message
	}
	return err.Error()
}
