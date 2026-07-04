package standing

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

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// validID is a well-formed volunteer UUID used across the handler tests.
const validID = "11111111-1111-1111-1111-111111111111"

// fakeRepo is an in-memory Repository double that records calls for handler tests.
type fakeRepo struct {
	entry *Entry
	list  []*Entry
	all   map[types.ID]Entry
	err   error

	setCalls   []setCall
	clearCalls []types.ID
	getCalls   []types.ID
	lastLimit  int
	lastOffset int
}

type setCall struct {
	id           types.ID
	standing     string
	benchedUntil *time.Time
	reason       string
}

func (f *fakeRepo) SetOperator(_ context.Context, id types.ID, standingValue string, benchedUntil *time.Time, reason string) (*Entry, error) {
	f.setCalls = append(f.setCalls, setCall{id, standingValue, benchedUntil, reason})
	return f.entry, f.err
}

func (f *fakeRepo) Clear(_ context.Context, id types.ID) (*Entry, error) {
	f.clearCalls = append(f.clearCalls, id)
	return f.entry, f.err
}

func (f *fakeRepo) Get(_ context.Context, id types.ID) (*Entry, error) {
	f.getCalls = append(f.getCalls, id)
	return f.entry, f.err
}

func (f *fakeRepo) ListNonOK(_ context.Context, limit, offset int) ([]*Entry, error) {
	f.lastLimit, f.lastOffset = limit, offset
	return f.list, f.err
}

func (f *fakeRepo) AllNonOK(_ context.Context) (map[types.ID]Entry, error) {
	return f.all, f.err
}

func testHandler(repo Repository) *Handler {
	return NewHandler(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// adminCtx returns a context carrying an admin caller.
func adminCtx() context.Context {
	return WithCaller(context.Background(), Caller{IsAdmin: true})
}

func TestHandleSet_NonAdminForbidden(t *testing.T) {
	repo := &fakeRepo{entry: &Entry{VolunteerID: types.MustParseID(validID), Standing: "BENCHED"}}
	h := testHandler(repo)

	// No caller injected at all (fails closed) and an explicit non-admin caller.
	for _, ctx := range []context.Context{
		context.Background(),
		WithCaller(context.Background(), Caller{IsAdmin: false}),
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/standing",
			strings.NewReader(`{"volunteer_id":"`+validID+`","standing":"BENCHED"}`)).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.HandleSet(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("non-admin HandleSet: status = %d, want 403", rec.Code)
		}
	}
	if len(repo.setCalls) != 0 {
		t.Errorf("SetOperator should not be called for non-admin, got %d calls", len(repo.setCalls))
	}
}

func TestHandleSet_Happy(t *testing.T) {
	until := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	repo := &fakeRepo{entry: &Entry{VolunteerID: types.MustParseID(validID), Standing: "BENCHED", Source: "OPERATOR"}}
	h := testHandler(repo)

	body := `{"volunteer_id":"` + validID + `","standing":"BENCHED","benched_until":"` +
		until.Format(time.RFC3339) + `","reason":"abuse"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/standing",
		strings.NewReader(body)).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleSet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(repo.setCalls) != 1 {
		t.Fatalf("SetOperator calls = %d, want 1", len(repo.setCalls))
	}
	got := repo.setCalls[0]
	if got.id != types.MustParseID(validID) || got.standing != "BENCHED" || got.reason != "abuse" {
		t.Errorf("SetOperator call = %+v, want id=%s standing=BENCHED reason=abuse", got, validID)
	}
	if got.benchedUntil == nil || !got.benchedUntil.Equal(until) {
		t.Errorf("benched_until = %v, want %v", got.benchedUntil, until)
	}
	var out Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Standing != "BENCHED" || out.Source != "OPERATOR" {
		t.Errorf("response entry = %+v, want standing=BENCHED source=OPERATOR", out)
	}
}

func TestHandleSet_ValidationErrors(t *testing.T) {
	h := testHandler(&fakeRepo{})
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"empty volunteer_id", `{"volunteer_id":"","standing":"OK"}`},
		{"bad uuid", `{"volunteer_id":"not-a-uuid","standing":"OK"}`},
		{"empty standing", `{"volunteer_id":"` + validID + `","standing":""}`},
		{"invalid standing", `{"volunteer_id":"` + validID + `","standing":"NOPE"}`},
		{"bad benched_until", `{"volunteer_id":"` + validID + `","standing":"BENCHED","benched_until":"soon"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/standing",
				strings.NewReader(tc.body)).WithContext(adminCtx())
			rec := httptest.NewRecorder()
			h.HandleSet(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestHandleSet_NotFound(t *testing.T) {
	// Repo returns (nil, nil) for an unknown volunteer.
	h := testHandler(&fakeRepo{entry: nil})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/standing",
		strings.NewReader(`{"volunteer_id":"`+validID+`","standing":"PROBATION"}`)).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleSet(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleClear_NonAdminForbidden(t *testing.T) {
	repo := &fakeRepo{entry: &Entry{VolunteerID: types.MustParseID(validID), Standing: "OK"}}
	h := testHandler(repo)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/standing/clear",
		strings.NewReader(`{"volunteer_id":"`+validID+`"}`)) // no caller
	rec := httptest.NewRecorder()
	h.HandleClear(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(repo.clearCalls) != 0 {
		t.Errorf("Clear should not be called for non-admin")
	}
}

func TestHandleClear_Happy(t *testing.T) {
	repo := &fakeRepo{entry: &Entry{VolunteerID: types.MustParseID(validID), Standing: "OK", Source: "AUTO"}}
	h := testHandler(repo)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/standing/clear",
		strings.NewReader(`{"volunteer_id":"`+validID+`"}`)).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleClear(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(repo.clearCalls) != 1 || repo.clearCalls[0] != types.MustParseID(validID) {
		t.Fatalf("Clear calls = %v, want [%s]", repo.clearCalls, validID)
	}
}

func TestHandleClear_BadUUID(t *testing.T) {
	h := testHandler(&fakeRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/standing/clear",
		strings.NewReader(`{"volunteer_id":"nope"}`)).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleClear(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleClear_NotFound(t *testing.T) {
	h := testHandler(&fakeRepo{entry: nil})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/standing/clear",
		strings.NewReader(`{"volunteer_id":"`+validID+`"}`)).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleClear(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleGet_NonAdminForbidden(t *testing.T) {
	h := testHandler(&fakeRepo{entry: &Entry{VolunteerID: types.MustParseID(validID), Standing: "PROBATION"}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/standing/"+validID, nil)
	req.SetPathValue("volunteer_id", validID)
	rec := httptest.NewRecorder()
	h.HandleGet(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestHandleGet_NotFound(t *testing.T) {
	h := testHandler(&fakeRepo{entry: nil})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/standing/"+validID, nil).
		WithContext(adminCtx())
	req.SetPathValue("volunteer_id", validID)
	rec := httptest.NewRecorder()
	h.HandleGet(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleGet_BadUUID(t *testing.T) {
	h := testHandler(&fakeRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/standing/nope", nil).
		WithContext(adminCtx())
	req.SetPathValue("volunteer_id", "nope")
	rec := httptest.NewRecorder()
	h.HandleGet(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGet_HappyIncludesEffectiveStanding(t *testing.T) {
	// A bench whose deadline has passed is stored BENCHED but reads as PROBATION.
	past := time.Now().Add(-time.Hour)
	h := testHandler(&fakeRepo{entry: &Entry{
		VolunteerID:  types.MustParseID(validID),
		Standing:     "BENCHED",
		BenchedUntil: &past,
		Source:       "OPERATOR",
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/standing/"+validID, nil).
		WithContext(adminCtx())
	req.SetPathValue("volunteer_id", validID)
	rec := httptest.NewRecorder()
	h.HandleGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Standing          string `json:"standing"`
		EffectiveStanding string `json:"effective_standing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Standing != "BENCHED" {
		t.Errorf("stored standing = %q, want BENCHED", got.Standing)
	}
	if got.EffectiveStanding != "PROBATION" {
		t.Errorf("effective_standing = %q, want PROBATION (expired bench)", got.EffectiveStanding)
	}
}

func TestHandleList_NonAdminForbidden(t *testing.T) {
	h := testHandler(&fakeRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/standing", nil)
	rec := httptest.NewRecorder()
	h.HandleList(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestHandleList_DefaultsAndCap(t *testing.T) {
	repo := &fakeRepo{list: []*Entry{{VolunteerID: types.MustParseID(validID), Standing: "BENCHED"}}}
	h := testHandler(repo)

	// Default limit.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/standing", nil).WithContext(adminCtx())
	rec := httptest.NewRecorder()
	h.HandleList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if repo.lastLimit != defaultListLimit || repo.lastOffset != 0 {
		t.Errorf("limit/offset = %d/%d, want %d/0", repo.lastLimit, repo.lastOffset, defaultListLimit)
	}

	// Over-cap limit is clamped; offset honored.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/standing?limit=99999&offset=7", nil).
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
		"/api/v1/admin/standing?limit=0",
		"/api/v1/admin/standing?limit=-1",
		"/api/v1/admin/standing?limit=abc",
		"/api/v1/admin/standing?offset=-1",
		"/api/v1/admin/standing?offset=xyz",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(adminCtx())
		rec := httptest.NewRecorder()
		h.HandleList(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
}
