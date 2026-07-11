package atproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/netguard"
)

// Typed errors that callers can match with errors.Is to distinguish an absent
// identity or record from a transport failure.
var (
	// ErrDIDNotFound is returned when a DID cannot be resolved (HTTP 404 from the
	// resolver). It signals the identity is gone, not that the network failed.
	ErrDIDNotFound = errors.New("atproto: DID not found")
	// ErrRecordNotFound is returned when the PDS reports the requested record
	// does not exist (XRPC "RecordNotFound").
	ErrRecordNotFound = errors.New("atproto: record not found")
	// ErrNoAuditLog is returned by PLCAuditLog for DID methods that do not
	// publish a PLC audit log (currently anything other than did:plc).
	ErrNoAuditLog = errors.New("atproto: audit log not available for this DID method")
)

// maxResponseBytes caps how much of a response body the client will read. The
// documents involved (DID documents, a single small record, an audit log) are
// modest; the cap only guards against a hostile or misbehaving endpoint.
const maxResponseBytes = 8 << 20 // 8 MiB

// Client is the head's ATProto surface. It is safe for concurrent use.
type Client struct {
	resolverURL string
	httpClient  *http.Client
	logger      *slog.Logger
}

// NewClient builds a Client. resolverURL is the base URL of the did:plc
// directory (e.g. "https://plc.directory"); trailing slashes are trimmed. When
// httpClient is nil a client with conservative dial/TLS/response timeouts is
// used — never the shared http.DefaultClient, which has no timeout. When logger
// is nil the default slog logger is used.
//
// The default client also installs the shared SSRF dial guard (internal/netguard):
// because PDS endpoints and did:web document URLs are volunteer-controlled, it
// refuses to connect to loopback, private, link-local, unspecified, multicast,
// CGNAT, NAT64, this-network, or IPv4-compatible-IPv6 addresses (see
// netguard.ErrDisallowedAddress). A caller that supplies its own httpClient
// BYPASSES this guard and takes on that responsibility — operators pointing the
// head at a private or development PDS, and tests using loopback servers, must
// supply their own client accordingly.
func NewClient(resolverURL string, httpClient *http.Client, logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		resolverURL: strings.TrimRight(resolverURL, "/"),
		httpClient:  httpClient,
		logger:      logger,
	}
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
				Control:   netguard.DialControl,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		},
	}
}

// Identity is the subset of a resolved DID document the head cares about.
type Identity struct {
	// DID is the resolved decentralized identifier.
	DID string
	// PDSEndpoint is the https base URL of the account's Personal Data Server.
	PDSEndpoint string
	// ATProtoSigningKey is the multibase-encoded key the network uses to sign the
	// account's repository commits. It may be empty if the document omits it.
	ATProtoSigningKey string
}

// Record is a single ATProto repository record as returned by getRecord.
type Record struct {
	URI   string
	CID   string
	Value json.RawMessage
}

// didDocument is the minimal shape of a DID document this client parses.
type didDocument struct {
	ID                 string `json:"id"`
	VerificationMethod []struct {
		ID                 string `json:"id"`
		Type               string `json:"type"`
		Controller         string `json:"controller"`
		PublicKeyMultibase string `json:"publicKeyMultibase"`
	} `json:"verificationMethod"`
	Service []struct {
		ID              string `json:"id"`
		Type            string `json:"type"`
		ServiceEndpoint string `json:"serviceEndpoint"`
	} `json:"service"`
}

// ResolveDID resolves a did:plc or did:web identifier to its DID document and
// extracts the account's PDS endpoint and ATProto signing key.
//
// did:plc identifiers are resolved by fetching {resolverURL}/{did}. did:web
// identifiers are translated to an https URL per the did:web method
// specification and fetched directly. The PDS endpoint is taken from the
// service entry whose id ends with "#atproto_pds" and whose type is
// "AtprotoPersonalDataServer"; its serviceEndpoint must be an https URL. The
// signing key is taken from the verification method whose id ends with
// "#atproto" and is tolerated to be absent. A 404 yields ErrDIDNotFound.
func (c *Client) ResolveDID(ctx context.Context, did string) (*Identity, error) {
	doc, err := c.fetchDIDDocument(ctx, did)
	if err != nil {
		return nil, err
	}

	id := &Identity{DID: did}

	for _, svc := range doc.Service {
		if strings.HasSuffix(svc.ID, "#atproto_pds") && svc.Type == "AtprotoPersonalDataServer" {
			if err := validateHTTPSURL(svc.ServiceEndpoint); err != nil {
				return nil, fmt.Errorf("resolve did %s: pds endpoint: %w", did, err)
			}
			id.PDSEndpoint = strings.TrimRight(svc.ServiceEndpoint, "/")
			break
		}
	}
	if id.PDSEndpoint == "" {
		return nil, fmt.Errorf("resolve did %s: DID document has no #atproto_pds service endpoint", did)
	}

	// The signing key is optional: a document may legitimately omit it.
	for _, vm := range doc.VerificationMethod {
		if strings.HasSuffix(vm.ID, "#atproto") {
			id.ATProtoSigningKey = vm.PublicKeyMultibase
			break
		}
	}

	return id, nil
}

func (c *Client) fetchDIDDocument(ctx context.Context, did string) (*didDocument, error) {
	var docURL string
	switch {
	case strings.HasPrefix(did, "did:plc:"):
		docURL = c.resolverURL + "/" + did
	case strings.HasPrefix(did, "did:web:"):
		u, err := didWebToURL(did)
		if err != nil {
			return nil, err
		}
		docURL = u
	default:
		return nil, fmt.Errorf("resolve did %s: unsupported DID method (want did:plc or did:web)", did)
	}

	resp, err := c.get(ctx, docURL)
	if err != nil {
		return nil, fmt.Errorf("resolve did %s: %w", did, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("resolve did %s: %w", did, ErrDIDNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resolve did %s: unexpected status %d: %s",
			did, resp.StatusCode, bodySnippet(resp.Body))
	}

	var doc didDocument
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("resolve did %s: decode DID document: %w", did, err)
	}
	return &doc, nil
}

// didWebToURL translates a did:web identifier into the https URL of its DID
// document per the did:web method specification: the colon-separated segments
// after "did:web:" are percent-decoded, the first is the host (which may carry a
// percent-encoded port), and remaining segments form a path. With no path the
// document lives at /.well-known/did.json; otherwise at {path}/did.json.
func didWebToURL(did string) (string, error) {
	const prefix = "did:web:"
	rest := strings.TrimPrefix(did, prefix)
	if rest == "" {
		return "", fmt.Errorf("resolve did %s: empty did:web identifier", did)
	}

	segments := strings.Split(rest, ":")
	for i, seg := range segments {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return "", fmt.Errorf("resolve did %s: percent-decode segment %q: %w", did, seg, err)
		}
		if dec == "" {
			return "", fmt.Errorf("resolve did %s: empty segment", did)
		}
		segments[i] = dec
	}

	var b strings.Builder
	b.WriteString("https://")
	b.WriteString(segments[0])
	if len(segments) == 1 {
		b.WriteString("/.well-known/did.json")
	} else {
		for _, seg := range segments[1:] {
			b.WriteString("/")
			b.WriteString(seg)
		}
		b.WriteString("/did.json")
	}
	return b.String(), nil
}

// GetRecord fetches a single record from a repository on the given PDS, without
// authentication. A PDS "RecordNotFound" XRPC error yields ErrRecordNotFound;
// other non-200 responses are wrapped, preserving the XRPC error name when the
// body is parseable.
func (c *Client) GetRecord(ctx context.Context, pdsURL, repoDID, collection, rkey string) (*Record, error) {
	q := url.Values{}
	q.Set("repo", repoDID)
	q.Set("collection", collection)
	q.Set("rkey", rkey)
	reqURL := strings.TrimRight(pdsURL, "/") + "/xrpc/com.atproto.repo.getRecord?" + q.Encode()

	resp, err := c.get(ctx, reqURL)
	if err != nil {
		return nil, fmt.Errorf("get record %s/%s/%s: %w", repoDID, collection, rkey, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var raw struct {
			URI   string          `json:"uri"`
			CID   string          `json:"cid"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&raw); err != nil {
			return nil, fmt.Errorf("get record %s/%s/%s: decode: %w", repoDID, collection, rkey, err)
		}
		return &Record{URI: raw.URI, CID: raw.CID, Value: raw.Value}, nil
	}

	xerr, raw := parseXRPCError(resp.Body)
	if strings.EqualFold(xerr.Error, "RecordNotFound") {
		return nil, fmt.Errorf("get record %s/%s/%s: %w", repoDID, collection, rkey, ErrRecordNotFound)
	}
	return nil, fmt.Errorf("get record %s/%s/%s: %s",
		repoDID, collection, rkey, describeXRPC(resp.StatusCode, xerr, raw))
}

// RepoAlive reports whether a repository is still present and served by the
// given PDS, using an unauthenticated describeRepo call.
//
// It returns (true, nil) on HTTP 200; (false, nil) when the XRPC error clearly
// indicates the repository or account is gone or gated (error names containing
// RepoNotFound, RepoDeactivated, RepoSuspended, RepoTakendown, AccountNotFound,
// AccountDeactivated, and similar); and (false, err) on a network failure, a 5xx
// response, or any non-200 the client cannot confidently read as "gone" — so a
// caller never mistakes a transient outage for a deleted account.
func (c *Client) RepoAlive(ctx context.Context, pdsURL, repoDID string) (bool, error) {
	q := url.Values{}
	q.Set("repo", repoDID)
	reqURL := strings.TrimRight(pdsURL, "/") + "/xrpc/com.atproto.repo.describeRepo?" + q.Encode()

	resp, err := c.get(ctx, reqURL)
	if err != nil {
		return false, fmt.Errorf("describe repo %s: %w", repoDID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}

	xerr, raw := parseXRPCError(resp.Body)
	if isRepoGoneError(xerr.Error) {
		return false, nil
	}
	if resp.StatusCode >= 500 {
		return false, fmt.Errorf("describe repo %s: %s", repoDID, describeXRPC(resp.StatusCode, xerr, raw))
	}
	// A non-200 the client cannot classify as "gone": surface it as an error
	// rather than silently reporting the repository absent.
	return false, fmt.Errorf("describe repo %s: %s", repoDID, describeXRPC(resp.StatusCode, xerr, raw))
}

// isRepoGoneError reports whether an XRPC error name denotes a repository or
// account that is deleted, deactivated, suspended, or otherwise gated. The
// match is case-insensitive and substring-based to tolerate namespacing.
func isRepoGoneError(name string) bool {
	n := strings.ToLower(name)
	for _, marker := range []string{
		"reponotfound",
		"repodeactivated",
		"reposuspended",
		"repotakendown",
		"repotakedown",
		"accountnotfound",
		"accountdeactivated",
		"accountsuspended",
		"accounttakendown",
		"accounttakedown",
	} {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}

// PLCOperation is one entry from a did:plc audit log, reduced to the fields the
// head watches for rotation: the ATProto signing key and PDS endpoint in effect
// after the operation. Fields that a legacy or unrecognized operation format
// does not carry are left empty rather than failing the whole log.
type PLCOperation struct {
	CreatedAt         time.Time
	ATProtoSigningKey string
	PDSEndpoint       string
	Raw               json.RawMessage
}

// PLCAuditLog fetches and parses the ordered audit log of a did:plc identifier
// from {resolverURL}/{did}/log/audit. It returns ErrNoAuditLog for DID methods
// that do not publish one (e.g. did:web) and ErrDIDNotFound when the resolver
// reports the DID unknown.
func (c *Client) PLCAuditLog(ctx context.Context, did string) ([]PLCOperation, error) {
	if !strings.HasPrefix(did, "did:plc:") {
		return nil, fmt.Errorf("audit log %s: %w", did, ErrNoAuditLog)
	}

	reqURL := c.resolverURL + "/" + did + "/log/audit"
	resp, err := c.get(ctx, reqURL)
	if err != nil {
		return nil, fmt.Errorf("audit log %s: %w", did, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("audit log %s: %w", did, ErrDIDNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audit log %s: unexpected status %d: %s",
			did, resp.StatusCode, bodySnippet(resp.Body))
	}

	var entries []auditEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&entries); err != nil {
		return nil, fmt.Errorf("audit log %s: decode: %w", did, err)
	}

	ops := make([]PLCOperation, 0, len(entries))
	for _, e := range entries {
		ops = append(ops, e.parse())
	}
	return ops, nil
}

// auditEntry is one raw audit-log element. createdAt sits on the entry; the
// signing key and PDS endpoint sit inside the nested operation.
type auditEntry struct {
	CreatedAt time.Time       `json:"createdAt"`
	Operation json.RawMessage `json:"operation"`
}

func (e auditEntry) parse() PLCOperation {
	op := PLCOperation{CreatedAt: e.CreatedAt, Raw: e.Operation}

	// Current PLC operation format. Legacy "create" operations use different
	// field names (signingKey/service); those simply do not populate these
	// fields, which is the intended tolerant behavior.
	var parsed struct {
		VerificationMethods map[string]string `json:"verificationMethods"`
		Services            map[string]struct {
			Type     string `json:"type"`
			Endpoint string `json:"endpoint"`
		} `json:"services"`
	}
	if err := json.Unmarshal(e.Operation, &parsed); err == nil {
		op.ATProtoSigningKey = parsed.VerificationMethods["atproto"]
		if svc, ok := parsed.Services["atproto_pds"]; ok {
			op.PDSEndpoint = svc.Endpoint
		}
	}
	return op
}

// get issues an unauthenticated GET with the client's timeouts.
func (c *Client) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	return resp, nil
}

// xrpcError is the standard XRPC error envelope: {"error":"Name","message":"…"}.
type xrpcError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// parseXRPCError reads and returns the XRPC error envelope along with the raw
// body, so callers can fall back to a body snippet when the body is not JSON.
func parseXRPCError(body io.Reader) (xrpcError, []byte) {
	raw, _ := io.ReadAll(io.LimitReader(body, maxResponseBytes))
	var e xrpcError
	_ = json.Unmarshal(raw, &e)
	return e, raw
}

// describeXRPC renders a human-readable description of a non-200 XRPC response,
// preferring the parsed error name and message and falling back to a raw
// snippet of the body.
func describeXRPC(status int, e xrpcError, raw []byte) string {
	switch {
	case e.Error != "" && e.Message != "":
		return fmt.Sprintf("xrpc status %d: %s: %s", status, e.Error, e.Message)
	case e.Error != "":
		return fmt.Sprintf("xrpc status %d: %s", status, e.Error)
	default:
		return fmt.Sprintf("xrpc status %d: %s", status, snippet(raw))
	}
}

// bodySnippet reads and truncates a response body for error messages.
func bodySnippet(body io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(body, 1<<12))
	return snippet(raw)
}

func snippet(raw []byte) string {
	const max = 256
	s := strings.TrimSpace(string(raw))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// validateHTTPSURL ensures a service endpoint is a syntactically valid https URL
// with a host, so the head never trusts a plaintext or malformed PDS endpoint.
func validateHTTPSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("endpoint %q is not https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q has no host", raw)
	}
	return nil
}
