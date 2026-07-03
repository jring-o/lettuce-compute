package atproto

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes caps how much of a PDS response we read into memory. Session
// and record responses are small; this only guards against a misbehaving server.
const maxResponseBytes = 4 * 1024 * 1024

// defaultTimeout bounds each XRPC call when the caller does not supply its own
// http.Client.
const defaultTimeout = 30 * time.Second

// Client is a minimal ATProto XRPC client scoped to the handful of
// com.atproto.* methods `bind-did` needs. After CreateSession succeeds it holds
// the authenticated DID and access token; the access token authorizes the
// repo-write methods and is never persisted.
type Client struct {
	pdsURL string
	http   *http.Client

	// DID is the account DID resolved by CreateSession. It is the permanent
	// identity for the account (the handle is only a mutable alias).
	DID string

	accessJwt string
}

// NewClient returns a Client for the given PDS base URL. A nil httpClient
// installs a default client with a request timeout.
func NewClient(pdsURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		pdsURL: strings.TrimRight(pdsURL, "/"),
		http:   httpClient,
	}
}

// sessionResponse is the subset of com.atproto.server.createSession we use.
type sessionResponse struct {
	DID       string `json:"did"`
	Handle    string `json:"handle"`
	AccessJwt string `json:"accessJwt"`
}

// CreateSession authenticates to the PDS with an app password and stores the
// resulting DID and access token on the Client. identifier may be a handle or a
// DID. The password is sent only in this request body and is never retained.
func (c *Client) CreateSession(ctx context.Context, identifier, appPassword string) error {
	var resp sessionResponse
	err := c.postXRPC(ctx, "com.atproto.server.createSession", map[string]string{
		"identifier": identifier,
		"password":   appPassword,
	}, false, &resp)
	if err != nil {
		return err
	}
	if resp.DID == "" || resp.AccessJwt == "" {
		return fmt.Errorf("createSession returned an incomplete session (missing did or accessJwt)")
	}
	c.DID = resp.DID
	c.accessJwt = resp.AccessJwt
	return nil
}

// Record is a single record returned by listRecords. Value is left raw so the
// caller can inspect only the fields it needs.
type Record struct {
	URI   string          `json:"uri"`
	CID   string          `json:"cid"`
	Value json.RawMessage `json:"value"`
}

// listRecordsResponse is the subset of com.atproto.repo.listRecords we use.
type listRecordsResponse struct {
	Records []Record `json:"records"`
	Cursor  string   `json:"cursor"`
}

// ListRecords returns up to limit records from the authenticated repo in the
// given collection.
func (c *Client) ListRecords(ctx context.Context, collection string, limit int) ([]Record, error) {
	q := url.Values{}
	q.Set("repo", c.DID)
	q.Set("collection", collection)
	q.Set("limit", strconv.Itoa(limit))
	var resp listRecordsResponse
	if err := c.getXRPC(ctx, "com.atproto.repo.listRecords", q, true, &resp); err != nil {
		return nil, err
	}
	return resp.Records, nil
}

// writeResult is the shared response shape of createRecord and putRecord.
type writeResult struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// CreateRecord creates a new record in the collection and returns its URI and
// CID. The PDS assigns the rkey.
func (c *Client) CreateRecord(ctx context.Context, collection string, record map[string]any) (uri, cid string, err error) {
	var resp writeResult
	err = c.postXRPC(ctx, "com.atproto.repo.createRecord", map[string]any{
		"repo":       c.DID,
		"collection": collection,
		"record":     record,
	}, true, &resp)
	if err != nil {
		return "", "", err
	}
	return resp.URI, resp.CID, nil
}

// PutRecord overwrites the record at the given rkey (or creates it at that rkey)
// and returns its URI and CID.
func (c *Client) PutRecord(ctx context.Context, collection, rkey string, record map[string]any) (uri, cid string, err error) {
	var resp writeResult
	err = c.postXRPC(ctx, "com.atproto.repo.putRecord", map[string]any{
		"repo":       c.DID,
		"collection": collection,
		"rkey":       rkey,
		"record":     record,
	}, true, &resp)
	if err != nil {
		return "", "", err
	}
	return resp.URI, resp.CID, nil
}

// BuildRecord assembles the keyAuthorization record value for a repo write. The
// keySignature rides as an ATProto bytes value using the {"$bytes": "<base64>"}
// JSON envelope (standard base64, per the ATProto data model). label is omitted
// when empty. collection is written as the record's "$type".
func BuildRecord(collection, did, operationalKey, label, createdAt string, keySignature []byte) map[string]any {
	record := map[string]any{
		"$type":          collection,
		"did":            did,
		"operationalKey": operationalKey,
		"createdAt":      createdAt,
		"keySignature": map[string]any{
			"$bytes": base64.StdEncoding.EncodeToString(keySignature),
		},
	}
	if label != "" {
		record["label"] = label
	}
	return record
}

// OperationalKeyOf extracts the operationalKey field from a record value, used
// to detect an existing binding for this device key. A record whose value does
// not carry the field yields "".
func OperationalKeyOf(value json.RawMessage) string {
	var v struct {
		OperationalKey string `json:"operationalKey"`
	}
	_ = json.Unmarshal(value, &v)
	return v.OperationalKey
}

// RkeyFromURI returns the record key (final path segment) of an at:// record URI.
func RkeyFromURI(uri string) string {
	i := strings.LastIndex(uri, "/")
	if i < 0 {
		return ""
	}
	return uri[i+1:]
}

func (c *Client) postXRPC(ctx context.Context, nsid string, reqBody any, auth bool, out any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshaling %s request: %w", nsid, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.pdsURL+"/xrpc/"+nsid, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building %s request: %w", nsid, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.accessJwt)
	}
	return c.do(req, nsid, out)
}

func (c *Client) getXRPC(ctx context.Context, nsid string, query url.Values, auth bool, out any) error {
	u := c.pdsURL + "/xrpc/" + nsid
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("building %s request: %w", nsid, err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.accessJwt)
	}
	return c.do(req, nsid, out)
}

func (c *Client) do(req *http.Request, nsid string, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed: %w", nsid, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("reading %s response: %w", nsid, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d: %s", nsid, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decoding %s response: %w", nsid, err)
		}
	}
	return nil
}
