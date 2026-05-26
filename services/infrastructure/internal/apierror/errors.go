package apierror

import "fmt"

// APIError represents a structured API error response.
type APIError struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Details    any   `json:"details,omitempty"`
	HTTPStatus int         `json:"-"`
	Err        error       `json:"-"`
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the wrapped error for errors.Is/As support.
func (e *APIError) Unwrap() error {
	return e.Err
}

// ErrorResponse wraps an APIError for JSON serialization.
type ErrorResponse struct {
	Error APIError `json:"error"`
}
