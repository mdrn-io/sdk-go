package mdrn

import (
	"encoding/json"
	"fmt"
)

// The codes the SDK raises itself, beside the codes of MDRN's error envelope (INVALID_REQUEST,
// CONFLICT, NOT_FOUND, TEMPLATE_NOT_FOUND, ...).
const (
	// CodeProductNotEnabled: the product has no URL or token in this environment; status 0,
	// raised before any request.
	CodeProductNotEnabled = "PRODUCT_NOT_ENABLED"
	// CodeTimeout: no answer within the client's timeout (10 s by default); status 0.
	CodeTimeout = "TIMEOUT"
	// CodeNetworkError: the request could not be sent or the response not read; status 0.
	CodeNetworkError = "NETWORK_ERROR"
	// CodeUnexpectedRedirect: MDRN answered with a 3xx, which the SDK never follows.
	CodeUnexpectedRedirect = "UNEXPECTED_REDIRECT"
	// CodeHTTPError: a failure whose body is not MDRN's error envelope.
	CodeHTTPError = "HTTP_ERROR"
	// CodeUploadFailed: an upload did not accept parts; status 0, or the status of the part PUT.
	CodeUploadFailed = "UPLOAD_FAILED"
	// CodeFileRejected: MDRN rejected the uploaded bytes (size or SHA-256 mismatch); status 0.
	CodeFileRejected = "FILE_REJECTED"
)

// Error is a call that did not succeed: a response MDRN answered with a failure, or a request
// that could not be made. Status is the HTTP status (0 when no response was received), Code
// MDRN's error code or one of the SDK's own (the Code* constants), Message MDRN's message (for
// your logs) and Details the response body when it was JSON. A transport failure's cause is
// available through errors.Unwrap.
type Error struct {
	Status  int
	Code    string
	Message string
	Details json.RawMessage
	cause   error
}

// Error gives MDRN's message, or a generic one when the body was not an MDRN error.
func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Status != 0 {
		return fmt.Sprintf("MDRN request failed with HTTP %d", e.Status)
	}
	return e.Code
}

// Unwrap gives the underlying transport error of a TIMEOUT or NETWORK_ERROR, else nil.
func (e *Error) Unwrap() error { return e.cause }

// ArgumentError is a call the SDK refused before sending anything, because what it was given
// cannot be sent: a zero version, a template together with content, a file without a name or
// bytes, a value that is not JSON.
type ArgumentError struct {
	Message string
}

func (e *ArgumentError) Error() string { return e.Message }

func argument(format string, args ...any) error {
	return &ArgumentError{Message: fmt.Sprintf(format, args...)}
}

// apiError reads {"error":{"code","message"}}, or a bare {"code","message"}, and falls back to
// HTTP_ERROR for any other body, as the other MDRN SDKs do.
func apiError(status int, raw []byte) *Error {
	type fields struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	var nested struct {
		Error json.RawMessage `json:"error"`
	}
	var found fields
	if json.Unmarshal(raw, &nested) == nil {
		if len(nested.Error) > 0 && nested.Error[0] == '{' {
			_ = json.Unmarshal(nested.Error, &found)
		} else if len(nested.Error) == 0 {
			_ = json.Unmarshal(raw, &found)
		}
	}
	if found.Code == "" {
		found.Code = CodeHTTPError
	}
	var details json.RawMessage
	if json.Valid(raw) {
		details = raw
	}
	return &Error{Status: status, Code: found.Code, Message: found.Message, Details: details}
}
