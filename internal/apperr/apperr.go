// Package apperr defines the structured error codes shared by the daemon,
// the HTTP API, the CLI, and the TUI.
package apperr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier.
type Code string

// Error codes from the specsheet.
const (
	FileNotFound       Code = "FILE_NOT_FOUND"
	UnsupportedFormat  Code = "UNSUPPORTED_FORMAT"
	DecodeError        Code = "DECODE_ERROR"
	InvalidPlaylist    Code = "INVALID_PLAYLIST"
	AliasNotFound      Code = "ALIAS_NOT_FOUND"
	NoMediaLoaded      Code = "NO_MEDIA_LOADED"
	PlaylistRequired   Code = "PLAYLIST_REQUIRED"
	InvalidState       Code = "INVALID_STATE"
	VolumeMax          Code = "VOLUME_MAX"
	VolumeMin          Code = "VOLUME_MIN"
	ServiceNotRunning  Code = "SERVICE_NOT_RUNNING"
	ServiceStartFailed Code = "SERVICE_START_FAILED"
	TUIRequiresTTY     Code = "TUI_REQUIRES_TTY"
	InvalidCommand     Code = "INVALID_COMMAND"
	AuthRequired       Code = "AUTH_REQUIRED"
	Unauthorized       Code = "UNAUTHORIZED"
	SinkUnavailable    Code = "SINK_UNAVAILABLE"
	DBError            Code = "DB_ERROR"
	Internal           Code = "INTERNAL_ERROR"
	AlreadyRunning     Code = "ALREADY_RUNNING"
	FileExists         Code = "FILE_EXISTS"
	Forbidden          Code = "FORBIDDEN_PATH"
	TooManyListeners   Code = "TOO_MANY_LISTENERS"
)

// Error is an error carrying a Code.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	cause   error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Unwrap returns the underlying cause, if any.
func (e *Error) Unwrap() error { return e.cause }

// New creates an Error with a formatted message.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap creates an Error that wraps cause.
func Wrap(code Code, cause error, format string, args ...any) *Error {
	msg := fmt.Sprintf(format, args...)
	if cause != nil {
		if msg != "" {
			msg += ": "
		}
		msg += cause.Error()
	}
	return &Error{Code: code, Message: msg, cause: cause}
}

// CodeOf extracts the Code of err, or Internal when err is not an *Error.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Internal
}

// Is reports whether err carries the given code.
func Is(err error, code Code) bool { return err != nil && CodeOf(err) == code }

// As converts any error into an *Error.
func As(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: Internal, Message: err.Error(), cause: err}
}

// HTTPStatus maps a code to an HTTP status code.
func HTTPStatus(code Code) int {
	switch code {
	case InvalidCommand:
		return http.StatusBadRequest
	case AuthRequired, Unauthorized:
		return http.StatusUnauthorized
	case Forbidden:
		return http.StatusForbidden
	case FileNotFound, AliasNotFound:
		return http.StatusNotFound
	case NoMediaLoaded, PlaylistRequired, InvalidState, VolumeMax, VolumeMin, FileExists:
		return http.StatusConflict
	case UnsupportedFormat:
		return http.StatusUnsupportedMediaType
	case InvalidPlaylist, DecodeError:
		return http.StatusUnprocessableEntity
	case SinkUnavailable, TooManyListeners:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Envelope is the JSON error body: {"error": {...}}.
type Envelope struct {
	Error *Error `json:"error"`
}

// MarshalEnvelope renders err as a JSON envelope.
func MarshalEnvelope(err error) []byte {
	b, _ := json.Marshal(Envelope{Error: As(err)})
	return b
}

// ExitCode maps an error to a process exit code. All errors are non-zero.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch CodeOf(err) {
	case InvalidCommand:
		return 2
	case ServiceNotRunning, ServiceStartFailed:
		return 3
	case AuthRequired, Unauthorized:
		return 4
	default:
		return 1
	}
}
