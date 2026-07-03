package atproto

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newResolver stands up a fake did:plc directory serving DID documents at
// /{did} and audit logs at /{did}/log/audit from the supplied maps.
func newResolver(docs, auditLogs map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if did, ok := strings.CutSuffix(path, "/log/audit"); ok {
			body, found := auditLogs[did]
			if !found {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
			return
		}
		body, found := docs[path]
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func newClient(resolverURL string) *Client {
	// httptest servers listen on loopback, which the default client's SSRF dial
	// guard refuses by design. Supplying an explicit client opts out of the guard
	// (see NewClient docs) and is exactly how an operator would reach a private or
	// development PDS.
	return NewClient(resolverURL, &http.Client{Timeout: 10 * time.Second}, nil)
}

func TestResolveDIDPLCHappyPath(t *testing.T) {
	const doc = `{
		"id": "did:plc:abc",
		"verificationMethod": [
			{"id":"did:plc:abc#atproto","type":"Multikey","controller":"did:plc:abc","publicKeyMultibase":"z6MkSigningKeyExample"}
		],
		"service": [
			{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.com/"}
		]
	}`
	srv := newResolver(map[string]string{"did:plc:abc": doc}, nil)
	defer srv.Close()

	id, err := newClient(srv.URL).ResolveDID(context.Background(), "did:plc:abc")
	if err != nil {
		t.Fatal(err)
	}
	if id.DID != "did:plc:abc" {
		t.Errorf("DID = %q", id.DID)
	}
	if id.PDSEndpoint != "https://pds.example.com" { // trailing slash trimmed
		t.Errorf("PDSEndpoint = %q", id.PDSEndpoint)
	}
	if id.ATProtoSigningKey != "z6MkSigningKeyExample" {
		t.Errorf("ATProtoSigningKey = %q", id.ATProtoSigningKey)
	}
}

func TestResolveDIDSigningKeyOptional(t *testing.T) {
	const doc = `{
		"id": "did:plc:abc",
		"service": [
			{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.com"}
		]
	}`
	srv := newResolver(map[string]string{"did:plc:abc": doc}, nil)
	defer srv.Close()

	id, err := newClient(srv.URL).ResolveDID(context.Background(), "did:plc:abc")
	if err != nil {
		t.Fatal(err)
	}
	if id.ATProtoSigningKey != "" {
		t.Errorf("expected empty signing key, got %q", id.ATProtoSigningKey)
	}
}

func TestResolveDIDNotFound(t *testing.T) {
	srv := newResolver(nil, nil)
	defer srv.Close()

	_, err := newClient(srv.URL).ResolveDID(context.Background(), "did:plc:missing")
	if !errors.Is(err, ErrDIDNotFound) {
		t.Fatalf("want ErrDIDNotFound, got %v", err)
	}
}

func TestResolveDIDMissingPDS(t *testing.T) {
	const doc = `{"id":"did:plc:abc","service":[]}`
	srv := newResolver(map[string]string{"did:plc:abc": doc}, nil)
	defer srv.Close()

	if _, err := newClient(srv.URL).ResolveDID(context.Background(), "did:plc:abc"); err == nil {
		t.Fatal("expected error when no #atproto_pds service is present")
	}
}

func TestResolveDIDRejectsNonHTTPSPDS(t *testing.T) {
	const doc = `{
		"id":"did:plc:abc",
		"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"http://pds.example.com"}]
	}`
	srv := newResolver(map[string]string{"did:plc:abc": doc}, nil)
	defer srv.Close()

	if _, err := newClient(srv.URL).ResolveDID(context.Background(), "did:plc:abc"); err == nil {
		t.Fatal("expected error for non-https PDS endpoint")
	}
}

func TestResolveDIDUnsupportedMethod(t *testing.T) {
	srv := newResolver(nil, nil)
	defer srv.Close()
	if _, err := newClient(srv.URL).ResolveDID(context.Background(), "did:example:abc"); err == nil {
		t.Fatal("expected error for unsupported DID method")
	}
}

func TestDIDWebTranslation(t *testing.T) {
	cases := []struct {
		did  string
		want string
	}{
		{"did:web:example.com", "https://example.com/.well-known/did.json"},
		{"did:web:example.com:user:alice", "https://example.com/user/alice/did.json"},
		{"did:web:example.com%3A3000", "https://example.com:3000/.well-known/did.json"},
		{"did:web:example.com%3A3000:path:to:doc", "https://example.com:3000/path/to/doc/did.json"},
	}
	for _, tc := range cases {
		got, err := didWebToURL(tc.did)
		if err != nil {
			t.Fatalf("didWebToURL(%q): %v", tc.did, err)
		}
		if got != tc.want {
			t.Errorf("didWebToURL(%q) = %q, want %q", tc.did, got, tc.want)
		}
	}

	if _, err := didWebToURL("did:web:"); err == nil {
		t.Error("expected error for empty did:web identifier")
	}
}

func TestGetRecordHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xrpc/com.atproto.repo.getRecord" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("repo") != "did:plc:abc" || q.Get("collection") != "app.x" || q.Get("rkey") != "self" {
			t.Errorf("unexpected query %v", q)
		}
		_, _ = w.Write([]byte(`{"uri":"at://did:plc:abc/app.x/self","cid":"bafyabc","value":{"hello":"world"}}`))
	}))
	defer srv.Close()

	rec, err := newClient("").GetRecord(context.Background(), srv.URL, "did:plc:abc", "app.x", "self")
	if err != nil {
		t.Fatal(err)
	}
	if rec.URI != "at://did:plc:abc/app.x/self" || rec.CID != "bafyabc" {
		t.Errorf("unexpected record %+v", rec)
	}
	var val map[string]string
	if err := json.Unmarshal(rec.Value, &val); err != nil {
		t.Fatal(err)
	}
	if val["hello"] != "world" {
		t.Errorf("unexpected value %v", val)
	}
}

func TestGetRecordNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"RecordNotFound","message":"Could not locate record"}`))
	}))
	defer srv.Close()

	_, err := newClient("").GetRecord(context.Background(), srv.URL, "did:plc:abc", "app.x", "self")
	if !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("want ErrRecordNotFound, got %v", err)
	}
}

func TestGetRecordOtherXRPCErrorPreservesName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"InvalidRequest","message":"bad collection"}`))
	}))
	defer srv.Close()

	_, err := newClient("").GetRecord(context.Background(), srv.URL, "did:plc:abc", "app.x", "self")
	if err == nil || errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("want a generic wrapped error, got %v", err)
	}
	if !strings.Contains(err.Error(), "InvalidRequest") {
		t.Errorf("error should preserve XRPC name, got %v", err)
	}
}

func TestRepoAlive(t *testing.T) {
	t.Run("alive on 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"did":"did:plc:abc","handle":"alice.test"}`))
		}))
		defer srv.Close()
		alive, err := newClient("").RepoAlive(context.Background(), srv.URL, "did:plc:abc")
		if err != nil || !alive {
			t.Fatalf("alive=%v err=%v", alive, err)
		}
	})

	goneNames := []string{"RepoNotFound", "RepoDeactivated", "RepoSuspended", "RepoTakendown", "AccountDeactivated"}
	for _, name := range goneNames {
		t.Run("gone: "+name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"` + name + `","message":"gone"}`))
			}))
			defer srv.Close()
			alive, err := newClient("").RepoAlive(context.Background(), srv.URL, "did:plc:abc")
			if err != nil {
				t.Fatalf("want (false,nil) for %s, got err %v", name, err)
			}
			if alive {
				t.Fatalf("want not alive for %s", name)
			}
		})
	}

	t.Run("5xx is an error, not absence", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"InternalServerError"}`))
		}))
		defer srv.Close()
		alive, err := newClient("").RepoAlive(context.Background(), srv.URL, "did:plc:abc")
		if err == nil {
			t.Fatal("want error on 5xx")
		}
		if alive {
			t.Fatal("want not alive on 5xx")
		}
	})

	t.Run("unclassified 4xx is an error, not absence", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"InvalidRequest"}`))
		}))
		defer srv.Close()
		_, err := newClient("").RepoAlive(context.Background(), srv.URL, "did:plc:abc")
		if err == nil {
			t.Fatal("want error for an unclassified 4xx")
		}
	})

	t.Run("network failure is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close() // force connection refused
		_, err := newClient("").RepoAlive(context.Background(), url, "did:plc:abc")
		if err == nil {
			t.Fatal("want error on network failure")
		}
	})
}

func TestPLCAuditLogParsesRotation(t *testing.T) {
	// Two operations where the atproto signing key and PDS endpoint change from
	// the first to the second — the rotation the head must be able to detect.
	const auditLog = `[
		{
			"createdAt":"2026-01-01T00:00:00Z",
			"operation":{
				"type":"plc_operation",
				"verificationMethods":{"atproto":"did:key:z6MkFirstKey"},
				"services":{"atproto_pds":{"type":"AtprotoPersonalDataServer","endpoint":"https://pds1.example.com"}}
			}
		},
		{
			"createdAt":"2026-06-01T00:00:00Z",
			"operation":{
				"type":"plc_operation",
				"verificationMethods":{"atproto":"did:key:z6MkSecondKey"},
				"services":{"atproto_pds":{"type":"AtprotoPersonalDataServer","endpoint":"https://pds2.example.com"}}
			}
		}
	]`
	srv := newResolver(nil, map[string]string{"did:plc:abc": auditLog})
	defer srv.Close()

	ops, err := newClient(srv.URL).PLCAuditLog(context.Background(), "did:plc:abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatalf("want 2 operations, got %d", len(ops))
	}
	if ops[0].ATProtoSigningKey != "did:key:z6MkFirstKey" || ops[1].ATProtoSigningKey != "did:key:z6MkSecondKey" {
		t.Errorf("signing keys: %q then %q", ops[0].ATProtoSigningKey, ops[1].ATProtoSigningKey)
	}
	if ops[0].PDSEndpoint != "https://pds1.example.com" || ops[1].PDSEndpoint != "https://pds2.example.com" {
		t.Errorf("pds endpoints: %q then %q", ops[0].PDSEndpoint, ops[1].PDSEndpoint)
	}
	if ops[0].ATProtoSigningKey == ops[1].ATProtoSigningKey {
		t.Error("expected a key rotation across the two operations")
	}
	if ops[0].CreatedAt.IsZero() {
		t.Error("createdAt not parsed")
	}
}

func TestPLCAuditLogToleratesLegacyOp(t *testing.T) {
	// A legacy "create" operation uses signingKey/service rather than
	// verificationMethods/services; the parser must not fail the whole log — it
	// leaves the unrecognized fields empty.
	const auditLog = `[
		{"createdAt":"2022-01-01T00:00:00Z","operation":{"type":"create","signingKey":"did:key:zLegacy","service":"https://legacy.example.com"}}
	]`
	srv := newResolver(nil, map[string]string{"did:plc:abc": auditLog})
	defer srv.Close()

	ops, err := newClient(srv.URL).PLCAuditLog(context.Background(), "did:plc:abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("want 1 op, got %d", len(ops))
	}
	if ops[0].ATProtoSigningKey != "" || ops[0].PDSEndpoint != "" {
		t.Errorf("legacy op should leave fields empty, got key=%q pds=%q", ops[0].ATProtoSigningKey, ops[0].PDSEndpoint)
	}
	if len(ops[0].Raw) == 0 {
		t.Error("raw operation should be preserved")
	}
}

func TestPLCAuditLogRejectsDIDWeb(t *testing.T) {
	if _, err := newClient("http://unused").PLCAuditLog(context.Background(), "did:web:example.com"); !errors.Is(err, ErrNoAuditLog) {
		t.Fatalf("want ErrNoAuditLog for did:web, got %v", err)
	}
}
