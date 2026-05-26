package apierror

import "fmt"

// ValidationError creates a 400 Bad Request error.
func ValidationError(message string, details any) *APIError {
	return &APIError{
		Code:       "VALIDATION_ERROR",
		Message:    message,
		Details:    details,
		HTTPStatus: 400,
	}
}

// Unauthorized creates a 401 Unauthorized error.
func Unauthorized(message string) *APIError {
	return &APIError{
		Code:       "UNAUTHORIZED",
		Message:    message,
		HTTPStatus: 401,
	}
}

// Forbidden creates a 403 Forbidden error.
func Forbidden(message string) *APIError {
	return &APIError{
		Code:       "FORBIDDEN",
		Message:    message,
		HTTPStatus: 403,
	}
}

// NotFound creates a 404 Not Found error.
func NotFound(resource string, id string) *APIError {
	return &APIError{
		Code:       "NOT_FOUND",
		Message:    fmt.Sprintf("%s not found: %s", resource, id),
		HTTPStatus: 404,
	}
}

// Conflict creates a 409 Conflict error for state transition violations.
func Conflict(message string, details any) *APIError {
	return &APIError{
		Code:       "CONFLICT",
		Message:    message,
		Details:    details,
		HTTPStatus: 409,
	}
}

// RateLimited creates a 429 Rate Limited error.
func RateLimited(retryAfterSeconds int) *APIError {
	return &APIError{
		Code:       "RATE_LIMITED",
		Message:    "rate limit exceeded",
		Details:    map[string]int{"retry_after": retryAfterSeconds},
		HTTPStatus: 429,
	}
}

// Internal creates a 500 Internal Server Error.
// Wraps the original error for logging but does not expose it in the response.
func Internal(message string, err error) *APIError {
	return &APIError{
		Code:       "INTERNAL_ERROR",
		Message:    message,
		HTTPStatus: 500,
		Err:        err,
	}
}
