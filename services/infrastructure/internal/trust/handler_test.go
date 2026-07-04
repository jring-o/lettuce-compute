package trust

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeRepo is an in-memory Repository double that records calls for handler tests.
type fakeRepo struct {
	entry *Entry
	list  []*Entry
	err   error

	setCalls   []setCall
	slashCalls []string
	accrue     []string
	lastLimit  int
	lastOffset int
}

type setCall struct {
	subject string
	score   int
}

func (f *fakeRepo) GetScore(_ context.Context, subject string) (int, error) {
	if f.entry != nil && f.entry.Subject == subject {
		return f.entry.Score, f.err
	}
	return 0, f.err
}

func (f *fakeRepo) Get(_ context.Context, _ string) (*Entry, error) {
	return f.entry, f.err
}

func (f *fakeRepo) SetScore(_ context.Context, subject string, score int) error {
	f.setCalls = append(f.setCalls, setCall{subject, score})
	return f.err
}

func (f *fakeRepo) AccrueCleanUnit(_ context.Context, subject string) error {
	f.accrue = append(f.accrue, subject)
	return f.err
}

func (f *fakeRepo) Slash(_ context.Context, subject string) error {
	f.slashCalls = append(f.slashCalls, subject)
	return f.err
}

func (f *fakeRepo) List(_ context.Context, limit, offset int) ([]*Entry, error) {
	f.lastLimit, f.lastOffset = limit, offset
	return f.list, f.err
}

func (f *fakeRepo) AllScores(_ context.Context) (map[string]int, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]int{}
	if f.entry != nil && f.entry.Score > 0 {
		out[f.entry.Subject] = f.entry.Score
	}
	return out, nil
}

func testHandler(repo Repository) *Handler {
	return NewHandler(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// adminCtx returns a context carrying an admin caller.
func adminCtx() context.Context {
	return WithCaller(context.Background(), Caller{IsAdmin: true})
}

func TestHandleSet_NonAdminForbidden(t *testing.T) {
	repo := &fakeRepo{}
	h := testHandler(repo)

	// No caller injected at all (fails closed) and an explicit non-admin caller.
	for _, ctx := range []context.Context{
		context.Background(),
		WithCaller(context.Background(), Caller{IsAdmin: false}),
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/trust",
			strings.NewReader(`{"subject":"did:plc:x","score":10}`)).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.HandleSet(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("non-admin HandleSet: status = %d, want 403", rec.Code)
		}
	}
	if len(repo.setCalls) != 0 {
		t.Errorf("SetScore should not be called for non-admin, got %d calls", len(repo.setCalls))
	}
}

func TestHandleSet_Happy(t *testing.T) {
	repo := &fakeRepo{entry: &Entry{Subject: "did:plc:x", Score: 10, CleanUnits: 0}}
	h := testHandler(repo)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/trust",
		strings.NewReader(`{"subject":"did:plc:x","score":10}`)).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleSet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(repo.setCalls) != 1 || repo.setCalls[0] != (setCall{"did:plc:x", 10}) {
		t.Fatalf("SetScore calls = %+v, want one {did:plc:x 10}", repo.setCalls)
	}
	var got Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Subject != "did:plc:x" || got.Score != 10 {
		t.Errorf("response entry = %+v, want subject=did:plc:x score=10", got)
	}
}

func TestHandleSet_ValidationErrors(t *testing.T) {
	h := testHandler(&fakeRepo{})
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"empty subject", `{"subject":"","score":1}`},
		{"whitespace subject", `{"subject":"   ","score":1}`},
		{"negative score", `{"subject":"did:plc:x","score":-1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/trust",
				strings.NewReader(tc.body)).WithContext(adminCtx())
			rec := httptest.NewRecorder()
			h.HandleSet(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestHandleSlash_NonAdminForbidden(t *testing.T) {
	repo := &fakeRepo{}
	h := testHandler(repo)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/trust/slash",
		strings.NewReader(`{"subject":"did:plc:x"}`)) // no caller
	rec := httptest.NewRecorder()
	h.HandleSlash(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(repo.slashCalls) != 0 {
		t.Errorf("Slash should not be called for non-admin")
	}
}

func TestHandleSlash_Happy(t *testing.T) {
	slashedAt := time.Now().UTC()
	repo := &fakeRepo{entry: &Entry{Subject: "vol:abc", Score: 0, CleanUnits: 4, SlashedAt: &slashedAt}}
	h := testHandler(repo)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/trust/slash",
		strings.NewReader(`{"subject":"vol:abc"}`)).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleSlash(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(repo.slashCalls) != 1 || repo.slashCalls[0] != "vol:abc" {
		t.Fatalf("Slash calls = %+v, want [vol:abc]", repo.slashCalls)
	}
}

func TestHandleSlash_MissingSubject(t *testing.T) {
	h := testHandler(&fakeRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/trust/slash",
		strings.NewReader(`{"subject":""}`)).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleSlash(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGet_NonAdminForbidden(t *testing.T) {
	h := testHandler(&fakeRepo{entry: &Entry{Subject: "did:plc:x", Score: 5}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/trust/did:plc:x", nil)
	req.SetPathValue("subject", "did:plc:x")
	rec := httptest.NewRecorder()
	h.HandleGet(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestHandleGet_NotFound(t *testing.T) {
	h := testHandler(&fakeRepo{entry: nil})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/trust/did:plc:absent", nil).
		WithContext(adminCtx())
	req.SetPathValue("subject", "did:plc:absent")
	rec := httptest.NewRecorder()
	h.HandleGet(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleGet_Happy(t *testing.T) {
	h := testHandler(&fakeRepo{entry: &Entry{Subject: "did:plc:x", Score: 5, CleanUnits: 5}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/trust/did:plc:x", nil).
		WithContext(adminCtx())
	req.SetPathValue("subject", "did:plc:x")
	rec := httptest.NewRecorder()
	h.HandleGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Subject != "did:plc:x" || got.Score != 5 || got.CleanUnits != 5 {
		t.Errorf("entry = %+v, unexpected", got)
	}
}

func TestHandleList_NonAdminForbidden(t *testing.T) {
	h := testHandler(&fakeRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/trust", nil)
	rec := httptest.NewRecorder()
	h.HandleList(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestHandleList_DefaultsAndCap(t *testing.T) {
	repo := &fakeRepo{list: []*Entry{{Subject: "a", Score: 3}, {Subject: "b", Score: 1}}}
	h := testHandler(repo)

	// Default limit.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/trust", nil).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if repo.lastLimit != defaultListLimit || repo.lastOffset != 0 {
		t.Errorf("limit/offset = %d/%d, want %d/0", repo.lastLimit, repo.lastOffset, defaultListLimit)
	}

	// Over-cap limit is clamped; offset honored.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/trust?limit=99999&offset=7", nil).
		WithContext(adminCtx())
	rec = httptest.NewRecorder()
	h.HandleList(rec, req)
	if repo.lastLimit != maxListLimit || repo.lastOffset != 7 {
		t.Errorf("limit/offset = %d/%d, want %d/7", repo.lastLimit, repo.lastOffset, maxListLimit)
	}
}

func TestHandleList_InvalidParams(t *testing.T) {
	h := testHandler(&fakeRepo{})
	for _, target := range []string{
		"/api/v1/admin/trust?limit=0",
		"/api/v1/admin/trust?limit=-1",
		"/api/v1/admin/trust?limit=abc",
		"/api/v1/admin/trust?offset=-1",
		"/api/v1/admin/trust?offset=xyz",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(adminCtx())
		rec := httptest.NewRecorder()
		h.HandleList(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
}
