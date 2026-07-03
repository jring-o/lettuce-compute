package atproto

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testDID       = "did:plc:testaccount"
	testAccessJwt = "test.access.jwt"
)

// fakePDS is an httptest server that emulates the com.atproto.* XRPC methods the
// client uses. It records the last decoded request body for each method so tests
// can assert on what the client sent.
type fakePDS struct {
	t          *testing.T
	server     *httptest.Server
	lastCreate map[string]any
	lastPut    map[string]any
}

func newFakePDS(t *testing.T) *fakePDS {
	f := &fakePDS{t: t}
	mux := http.NewServeMux()

	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Identifier string `json:"identifier"`
			Password   string `json:"password"`
		}
		decodeBody(t, r, &body)
		if body.Identifier == "" || body.Password == "" {
			http.Error(w, `{"error":"InvalidRequest"}`, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{
			"did":       testDID,
			"handle":    "alice.test",
			"accessJwt": testAccessJwt,
		})
	})

	mux.HandleFunc("/xrpc/com.atproto.repo.listRecords", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		if got := r.URL.Query().Get("repo"); got != testDID {
			t.Errorf("listRecords repo = %q, want %q", got, testDID)
		}
		if got := r.URL.Query().Get("collection"); got != defaultKeyAuthorizationCollectionForTest {
			t.Errorf("listRecords collection = %q, want %q", got, defaultKeyAuthorizationCollectionForTest)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("listRecords limit = %q, want 100", got)
		}
		writeJSON(w, map[string]any{
			"records": []map[string]any{
				{
					"uri":   "at://" + testDID + "/" + defaultKeyAuthorizationCollectionForTest + "/existingrkey",
					"cid":   "bafyexisting",
					"value": map[string]any{"operationalKey": "did:key:zExisting"},
				},
			},
		})
	})

	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		f.lastCreate = map[string]any{}
		decodeBody(t, r, &f.lastCreate)
		writeJSON(w, map[string]string{
			"uri": "at://" + testDID + "/" + defaultKeyAuthorizationCollectionForTest + "/newrkey",
			"cid": "bafycreated",
		})
	})

	mux.HandleFunc("/xrpc/com.atproto.repo.putRecord", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		f.lastPut = map[string]any{}
		decodeBody(t, r, &f.lastPut)
		writeJSON(w, map[string]string{
			"uri": "at://" + testDID + "/" + defaultKeyAuthorizationCollectionForTest + "/existingrkey",
			"cid": "bafyput",
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// defaultKeyAuthorizationCollectionForTest mirrors the CLI default collection so
// the atproto package tests need no dependency on the cli package.
const defaultKeyAuthorizationCollectionForTest = "tech.scios.lettuce.keyAuthorization"

func TestCreateSession(t *testing.T) {
	f := newFakePDS(t)
	c := NewClient(f.server.URL, f.server.Client())

	if err := c.CreateSession(context.Background(), "alice.test", "app-pw-1234"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if c.DID != testDID {
		t.Errorf("DID = %q, want %q", c.DID, testDID)
	}
	if c.accessJwt != testAccessJwt {
		t.Errorf("accessJwt = %q, want %q", c.accessJwt, testAccessJwt)
	}
}

func TestListRecords(t *testing.T) {
	f := newFakePDS(t)
	c := NewClient(f.server.URL, f.server.Client())
	if err := c.CreateSession(context.Background(), "alice.test", "app-pw"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	recs, err := c.ListRecords(context.Background(), defaultKeyAuthorizationCollectionForTest, 100)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if got := OperationalKeyOf(recs[0].Value); got != "did:key:zExisting" {
		t.Errorf("OperationalKeyOf = %q, want did:key:zExisting", got)
	}
	if got := RkeyFromURI(recs[0].URI); got != "existingrkey" {
		t.Errorf("RkeyFromURI = %q, want existingrkey", got)
	}
}

func TestCreateRecordCarriesBytesEnvelope(t *testing.T) {
	f := newFakePDS(t)
	c := NewClient(f.server.URL, f.server.Client())
	if err := c.CreateSession(context.Background(), "alice.test", "app-pw"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	sig := []byte("a-signature-value")
	record := BuildRecord(defaultKeyAuthorizationCollectionForTest, testDID, "did:key:zNew", "workstation", "2026-07-03T12:00:00Z", sig)

	uri, cid, err := c.CreateRecord(context.Background(), defaultKeyAuthorizationCollectionForTest, record)
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if !strings.HasSuffix(uri, "/newrkey") || cid != "bafycreated" {
		t.Fatalf("unexpected create result uri=%q cid=%q", uri, cid)
	}

	// The server saw the record with the keySignature as a {"$bytes": base64} envelope.
	sent := f.lastCreate
	if sent["repo"] != testDID {
		t.Errorf("createRecord repo = %v, want %q", sent["repo"], testDID)
	}
	if sent["collection"] != defaultKeyAuthorizationCollectionForTest {
		t.Errorf("createRecord collection = %v", sent["collection"])
	}
	value, ok := sent["record"].(map[string]any)
	if !ok {
		t.Fatalf("createRecord record is not an object: %T", sent["record"])
	}
	if value["$type"] != defaultKeyAuthorizationCollectionForTest {
		t.Errorf("record $type = %v", value["$type"])
	}
	if value["label"] != "workstation" {
		t.Errorf("record label = %v, want workstation", value["label"])
	}
	envelope, ok := value["keySignature"].(map[string]any)
	if !ok {
		t.Fatalf("keySignature is not an object: %T", value["keySignature"])
	}
	b64, ok := envelope["$bytes"].(string)
	if !ok {
		t.Fatalf("keySignature.$bytes is not a string: %T", envelope["$bytes"])
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("keySignature.$bytes is not valid std base64: %v", err)
	}
	if string(decoded) != string(sig) {
		t.Errorf("decoded signature = %q, want %q", decoded, sig)
	}
}

func TestBuildRecordOmitsEmptyLabel(t *testing.T) {
	record := BuildRecord(defaultKeyAuthorizationCollectionForTest, testDID, "did:key:zNew", "", "2026-07-03T12:00:00Z", []byte("sig"))
	if _, present := record["label"]; present {
		t.Errorf("record should omit empty label, got %v", record["label"])
	}
}

func TestPutRecordSendsRkey(t *testing.T) {
	f := newFakePDS(t)
	c := NewClient(f.server.URL, f.server.Client())
	if err := c.CreateSession(context.Background(), "alice.test", "app-pw"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	record := BuildRecord(defaultKeyAuthorizationCollectionForTest, testDID, "did:key:zNew", "", "2026-07-03T12:00:00Z", []byte("sig"))
	uri, cid, err := c.PutRecord(context.Background(), defaultKeyAuthorizationCollectionForTest, "existingrkey", record)
	if err != nil {
		t.Fatalf("PutRecord: %v", err)
	}
	if !strings.HasSuffix(uri, "/existingrkey") || cid != "bafyput" {
		t.Fatalf("unexpected put result uri=%q cid=%q", uri, cid)
	}
	if f.lastPut["rkey"] != "existingrkey" {
		t.Errorf("putRecord rkey = %v, want existingrkey", f.lastPut["rkey"])
	}
}

func TestNon2xxIncludesBody(t *testing.T) {
	f := newFakePDS(t)
	c := NewClient(f.server.URL, f.server.Client())
	// Empty identifier triggers the fake's 400 with a body.
	err := c.CreateSession(context.Background(), "", "")
	if err == nil {
		t.Fatal("expected error from createSession with empty identifier")
	}
	if !strings.Contains(err.Error(), "InvalidRequest") {
		t.Errorf("error should include response body, got: %v", err)
	}
}

func decodeBody(t *testing.T, r *http.Request, out any) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading request body: %v", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decoding request body %q: %v", body, err)
	}
}

func requireBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+testAccessJwt {
		t.Errorf("Authorization = %q, want Bearer %s", got, testAccessJwt)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
