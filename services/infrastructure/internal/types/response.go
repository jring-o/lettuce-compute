package types

// ListResponse is the standard envelope for paginated list responses.
type ListResponse[T any] struct {
	Data       []T                `json:"data"`
	Pagination PaginationResponse `json:"pagination"`
}

// NewListResponse creates a ListResponse from a slice and pagination info.
// If data is nil, it is replaced with an empty slice to ensure JSON marshaling
// produces [] instead of null.
func NewListResponse[T any](data []T, pagination PaginationResponse) ListResponse[T] {
	if data == nil {
		data = []T{}
	}
	return ListResponse[T]{
		Data:       data,
		Pagination: pagination,
	}
}
