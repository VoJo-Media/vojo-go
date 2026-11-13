package vojo

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sentinels for use with errors.Is. An *APIError matches the one for its status.
var (
	ErrAuthentication = errors.New("vojo: authentication")    // 401
	ErrPermission     = errors.New("vojo: permission denied") // 402, 403
	ErrNotFound       = errors.New("vojo: not found")         // 404
	ErrValidation     = errors.New("vojo: validation failed") // 400, 413, 422
	ErrConflict       = errors.New("vojo: conflict")          // 409
	ErrRateLimit      = errors.New("vojo: rate limited")      // 429
	ErrServer         = errors.New("vojo: server error")      // 5xx
	// ErrPagination means the API returned paging data that cannot be followed safely.
	ErrPagination = errors.New("vojo: pagination")
)

var keyInText = regexp.MustCompile(`vojo_(live|test)_\w+`)

func redact(text string) string { return keyInText.ReplaceAllString(text, "<redacted>") }

// FieldError is one field-level reason on a validation failure.
type FieldError struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code"`
	Detail  string `json:"detail"`
}

// APIError is an error status from the API. Get one with errors.As:
//
//	var apiErr *vojo.APIError
//	if errors.As(err, &apiErr) { log.Print(apiErr.Code, apiErr.RequestID) }
//
// Branch on Code, or use errors.Is with the sentinels. New codes can appear
// within v1, so always handle the unknown case.
type APIError struct {
	Status    int
	Code      string // Empty when a proxy, not the API, produced the response.
	Message   string
	RequestID string
	Errors    []FieldError // Set on validation failures.
	// Problem is the full RFC 9457 body, including members this version of the
	// library does not know about. On a 402 it holds feature_key, limit and used.
	Problem map[string]any
	Header  http.Header
}

func (e *APIError) Error() string {
	text := "vojo: " + strconv.Itoa(e.Status)
	if e.Code != "" {
		text += " " + e.Code
	}
	text += ": " + e.Message
	if e.RequestID != "" {
		text += " (request id: " + e.RequestID + ")"
	}
	return text
}

// Is matches the sentinel for this error's status.
func (e *APIError) Is(target error) bool {
	if isValidationStatus(e.Status) {
		return target == ErrValidation
	}
	switch e.Status {
	case 401:
		return target == ErrAuthentication
	case 402, 403:
		return target == ErrPermission
	case 404:
		return target == ErrNotFound
	case 409:
		return target == ErrConflict
	case 429:
		return target == ErrRateLimit
	}
	return e.Status >= 500 && target == ErrServer
}

// RetryAfter is the wait the API asked for, if it gave one.
func (e *APIError) RetryAfter() (time.Duration, bool) {
	return parseRetryAfter(e.Header.Get("Retry-After"))
}

// ConnectionError means the request produced no response. Timeout is true when
// it timed out. It unwraps to the underlying error, such as context.Canceled.
type ConnectionError struct {
	Timeout bool
	Err     error
}

func (e *ConnectionError) Error() string {
	if e.Timeout {
		return redact("vojo: the request timed out: " + e.Err.Error())
	}
	return redact("vojo: could not reach the API: " + e.Err.Error())
}

func (e *ConnectionError) Unwrap() error { return e.Err }

var retryAfterPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// Past this a time.Duration overflows. It is far beyond any wait that is honoured.
const mostRetryAfterSeconds = 1e9

// parseRetryAfter reads a plain number of seconds. Anything else counts as not given.
func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if !retryAfterPattern.MatchString(value) {
		return 0, false
	}
	// The pattern leaves only one way to fail: digits too long for a float64.
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds > mostRetryAfterSeconds {
		seconds = mostRetryAfterSeconds
	}
	return time.Duration(seconds * float64(time.Second)), true
}
