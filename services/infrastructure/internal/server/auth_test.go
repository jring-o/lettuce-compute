package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- Mock API key repository ---

type mockApiKeyRepo struct {
	getByHashFn    func(ctx context.Context, keyHash []byte) (*apikey.ApiKey, error)
	updateLastUsed func(ctx context.Context, id types.ID) error
}

func (m *mockApiKeyRepo) Create(ctx context.Context, key *apikey.ApiKey) error { return nil }
func (m *mockApiKeyRepo) GetByHash(ctx context.Context, keyHash []byte) (*apikey.ApiKey, error) {
	if m.getByHashFn != nil {
		return m.getByHashFn(ctx, keyHash)
	}
	return nil, nil
}
func (m *mockApiKeyRepo) ListByUser(ctx context.Context, userID types.ID) ([]*apikey.ApiKey, error) {
	return nil, nil
}
func (m *mockApiKeyRepo) Revoke(ctx context.Context, id types.ID) error { return nil }
func (m *mockApiKeyRepo) UpdateLastUsed(ctx context.Context, id types.ID) error {
	if m.updateLastUsed != nil {
		return m.updateLastUsed(ctx, id)
	}
	return nil
}

// --- Mock project repository ---

type mockLeafRepo struct {
	getByIDFn func(ctx context.Context, id types.ID) (*leaf.Leaf, error)
}

func (m *mockLeafRepo) Create(ctx context.Context, p *leaf.Leaf) error { return nil }
func (m *mockLeafRepo) GetByID(ctx context.Context, id types.ID) (*leaf.Leaf, error) {
	if m.getByIDFn != nil {
		return m.getByIDFn(ctx, id)
	}
	return nil, apierror.NotFound("project", id.String())
}
func (m *mockLeafRepo) GetBySlug(ctx context.Context, slug string, creatorID *types.ID) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *mockLeafRepo) GetBySlugPublic(ctx context.Context, slug string) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *mockLeafRepo) List(ctx context.Context, filters leaf.LeafListFilters, page types.PaginationRequest) ([]*leaf.Leaf, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockLeafRepo) Update(ctx context.Context, p *leaf.Leaf) error { return nil }
func (m *mockLeafRepo) Delete(ctx context.Context, id types.ID) error        { return nil }

// --- Auth middleware tests ---

func TestAuthMiddleware_AnonymousPassesThrough(t *testing.T) {
	repo := &mockApiKeyRepo{}
	var capturedUser *AuthUser

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := authMiddleware(inner, repo, "test-admin-key", nil)

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedUser != nil {
		t.Fatal("expected nil user for anonymous request")
	}
}

func TestAuthMiddleware_ValidAPIKey(t *testing.T) {
	userID := types.NewID()
	repo := &mockApiKeyRepo{
		getByHashFn: func(ctx context.Context, keyHash []byte) (*apikey.ApiKey, error) {
			return &apikey.ApiKey{
				ID:     types.NewID(),
				UserID: userID,
				Name:   "test-key",
			}, nil
		},
	}

	var capturedUser *AuthUser
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := authMiddleware(inner, repo, "test-admin-key", nil)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer lk_somevalidkey123")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedUser == nil {
		t.Fatal("expected user in context")
	}
	if capturedUser.ID != userID {
		t.Fatalf("expected user ID %s, got %s", userID, capturedUser.ID)
	}
	if capturedUser.Role != "USER" {
		t.Fatalf("expected role USER, got %s", capturedUser.Role)
	}
}

func TestAuthMiddleware_RevokedKeyReturns401(t *testing.T) {
	// GetByHash returns nil for revoked keys (the query filters revoked_at IS NULL).
	repo := &mockApiKeyRepo{
		getByHashFn: func(ctx context.Context, keyHash []byte) (*apikey.ApiKey, error) {
			return nil, nil // Revoked = not found by hash
		},
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := authMiddleware(inner, repo, "test-admin-key", nil)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer some-revoked-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_NonBearerPassesThroughAsAnonymous(t *testing.T) {
	repo := &mockApiKeyRepo{}
	var capturedUser *AuthUser
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := authMiddleware(inner, repo, "test-admin-key", nil)

	// Non-Bearer schemes pass through as anonymous (e.g., Ed25519 auth is handled at handler level).
	tests := []struct {
		name   string
		header string
	}{
		{"non-bearer prefix", "Basic abc123"},
		{"bearer no token", "Bearer "},
		{"bearer only", "Bearer"},
		{"just token", "abc123"},
		{"ed25519 scheme", "Ed25519 pub:sig:123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capturedUser = nil
			req := httptest.NewRequest("GET", "/test", nil)
			req.Header.Set("Authorization", tt.header)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 (anonymous pass-through) for %q, got %d", tt.header, w.Code)
			}
			if capturedUser != nil {
				t.Fatalf("expected no user for %q, got %+v", tt.header, capturedUser)
			}
		})
	}
}

func TestAuthMiddleware_AdminEnvVarKey(t *testing.T) {
	adminKey := "super-secret-admin-key-12345"
	repo := &mockApiKeyRepo{
		getByHashFn: func(ctx context.Context, keyHash []byte) (*apikey.ApiKey, error) {
			return nil, nil // Not in DB
		},
	}

	var capturedUser *AuthUser
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := authMiddleware(inner, repo, adminKey, nil)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedUser == nil {
		t.Fatal("expected admin user in context")
	}
	if capturedUser.Role != "ADMIN" {
		t.Fatalf("expected role ADMIN, got %s", capturedUser.Role)
	}
	if capturedUser.Username != "admin" {
		t.Fatalf("expected username admin, got %s", capturedUser.Username)
	}
}

func TestAuthMiddleware_InvalidKeyReturns401(t *testing.T) {
	repo := &mockApiKeyRepo{
		getByHashFn: func(ctx context.Context, keyHash []byte) (*apikey.ApiKey, error) {
			return nil, nil
		},
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := authMiddleware(inner, repo, "real-admin-key", nil)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer totally-invalid-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// --- requireAuth tests ---

func TestRequireAuth_NoUserReturns401(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := requireAuth(inner)

	req := httptest.NewRequest("POST", "/api/v1/leafs", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	var resp apierror.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if resp.Error.Code != "UNAUTHORIZED" {
		t.Fatalf("expected UNAUTHORIZED, got %s", resp.Error.Code)
	}
}

func TestRequireAuth_WithUserPassesThrough(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := requireAuth(inner)

	req := httptest.NewRequest("POST", "/api/v1/leafs", nil)
	ctx := ContextWithUser(req.Context(), &AuthUser{
		ID:   types.NewID(),
		Role: "USER",
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Fatal("expected inner handler to be called")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- requireLeafOwnership tests ---

func TestRequireProjectOwnership_CreatorCanAccess(t *testing.T) {
	creatorID := types.NewID()
	leafID := types.NewID()

	leafRepo := &mockLeafRepo{
		getByIDFn: func(ctx context.Context, id types.ID) (*leaf.Leaf, error) {
			return &leaf.Leaf{
				ID:        leafID,
				CreatorID: &creatorID,
			}, nil
		},
	}

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := requireLeafOwnership(inner, leafRepo)

	// Use a ServeMux to properly set path values.
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", handler)

	req := httptest.NewRequest("PUT", "/api/v1/leafs/"+leafID.String(), nil)
	ctx := ContextWithUser(req.Context(), &AuthUser{
		ID:   creatorID,
		Role: "USER",
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if !called {
		t.Fatal("expected inner handler to be called")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestRequireProjectOwnership_NonCreatorGetsForbidden(t *testing.T) {
	creatorID := types.NewID()
	otherUserID := types.NewID()
	leafID := types.NewID()

	leafRepo := &mockLeafRepo{
		getByIDFn: func(ctx context.Context, id types.ID) (*leaf.Leaf, error) {
			return &leaf.Leaf{
				ID:        leafID,
				CreatorID: &creatorID,
			}, nil
		},
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := requireLeafOwnership(inner, leafRepo)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", handler)

	req := httptest.NewRequest("PUT", "/api/v1/leafs/"+leafID.String(), nil)
	ctx := ContextWithUser(req.Context(), &AuthUser{
		ID:   otherUserID,
		Role: "USER",
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}

	var resp apierror.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if resp.Error.Code != "FORBIDDEN" {
		t.Fatalf("expected FORBIDDEN, got %s", resp.Error.Code)
	}
}

func TestRequireProjectOwnership_AdminBypassesOwnership(t *testing.T) {
	creatorID := types.NewID()
	adminUserID := types.NewID()
	leafID := types.NewID()

	leafRepo := &mockLeafRepo{
		getByIDFn: func(ctx context.Context, id types.ID) (*leaf.Leaf, error) {
			return &leaf.Leaf{
				ID:        leafID,
				CreatorID: &creatorID,
			}, nil
		},
	}

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := requireLeafOwnership(inner, leafRepo)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", handler)

	req := httptest.NewRequest("PUT", "/api/v1/leafs/"+leafID.String(), nil)
	ctx := ContextWithUser(req.Context(), &AuthUser{
		ID:   adminUserID,
		Role: "ADMIN",
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if !called {
		t.Fatal("expected inner handler to be called for admin")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestRequireProjectOwnership_InvalidProjectID(t *testing.T) {
	leafRepo := &mockLeafRepo{}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := requireLeafOwnership(inner, leafRepo)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", handler)

	req := httptest.NewRequest("PUT", "/api/v1/leafs/not-a-uuid", nil)
	ctx := ContextWithUser(req.Context(), &AuthUser{
		ID:   types.NewID(),
		Role: "USER",
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- parseBearerToken tests ---

func TestParseBearerToken(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		wantOk   bool
		wantToken string
	}{
		{"valid", "Bearer mytoken123", true, "mytoken123"},
		{"case insensitive", "bearer mytoken123", true, "mytoken123"},
		{"no prefix", "mytoken123", false, ""},
		{"wrong prefix", "Basic mytoken123", false, ""},
		{"empty token", "Bearer ", false, ""},
		{"bearer only", "Bearer", false, ""},
		{"empty string", "", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, ok := parseBearerToken(tt.header)
			if ok != tt.wantOk {
				t.Fatalf("parseBearerToken(%q): ok=%v, want %v", tt.header, ok, tt.wantOk)
			}
			if token != tt.wantToken {
				t.Fatalf("parseBearerToken(%q): token=%q, want %q", tt.header, token, tt.wantToken)
			}
		})
	}
}

// --- ContextWithUser / UserFromContext tests ---

func TestContextUserRoundtrip(t *testing.T) {
	user := &AuthUser{
		ID:       types.NewID(),
		Email:    "test@example.com",
		Username: "testuser",
		Role:     "USER",
	}

	ctx := ContextWithUser(context.Background(), user)
	got := UserFromContext(ctx)

	if got == nil {
		t.Fatal("expected user from context")
	}
	if got.ID != user.ID {
		t.Fatalf("expected ID %s, got %s", user.ID, got.ID)
	}
}

func TestUserFromContext_NoUser(t *testing.T) {
	got := UserFromContext(context.Background())
	if got != nil {
		t.Fatal("expected nil user from empty context")
	}
}

// --- Gap tests: error paths ---

func TestAuthMiddleware_DBErrorReturns500(t *testing.T) {
	repo := &mockApiKeyRepo{
		getByHashFn: func(ctx context.Context, keyHash []byte) (*apikey.ApiKey, error) {
			return nil, apierror.Internal("database unavailable", nil)
		},
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := authMiddleware(inner, repo, "admin-key", logger)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer some-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestRequireProjectOwnership_ProjectNotFound(t *testing.T) {
	leafID := types.NewID()

	leafRepo := &mockLeafRepo{
		getByIDFn: func(ctx context.Context, id types.ID) (*leaf.Leaf, error) {
			return nil, apierror.NotFound("project", id.String())
		},
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := requireLeafOwnership(inner, leafRepo)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", handler)

	req := httptest.NewRequest("PUT", "/api/v1/leafs/"+leafID.String(), nil)
	ctx := ContextWithUser(req.Context(), &AuthUser{
		ID:   types.NewID(),
		Role: "USER",
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestRequireProjectOwnership_NilCreatorID(t *testing.T) {
	leafID := types.NewID()

	leafRepo := &mockLeafRepo{
		getByIDFn: func(ctx context.Context, id types.ID) (*leaf.Leaf, error) {
			return &leaf.Leaf{
				ID:        leafID,
				CreatorID: nil, // No creator set.
			}, nil
		},
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := requireLeafOwnership(inner, leafRepo)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", handler)

	req := httptest.NewRequest("PUT", "/api/v1/leafs/"+leafID.String(), nil)
	ctx := ContextWithUser(req.Context(), &AuthUser{
		ID:   types.NewID(),
		Role: "USER",
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for nil CreatorID, got %d", w.Code)
	}
}
