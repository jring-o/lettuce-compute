package apierror

import (
	"encoding/json"
	"errors"
	"net/http"
)

// WriteError writes an APIError as a JSON HTTP response.
func WriteError(w http.ResponseWriter, apiErr *APIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(apiErr.HTTPStatus)

	resp := ErrorResponse{Error: *apiErr}
	_ = json.NewEncoder(w).Encode(resp)
}

// FromError converts a generic error to an APIError.
// If the error is already an *APIError, returns it as-is.
// Otherwise, wraps it as an Internal error.
func FromError(err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return Internal("internal server error", err)
}
