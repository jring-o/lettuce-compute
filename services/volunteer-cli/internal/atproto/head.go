package atproto

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// bindDIDPath is the head REST route that records a device-key binding.
const bindDIDPath = "/api/v1/identity/bind-did"

// SignEd25519Request sets the Authorization header the Lettuce head's Ed25519
// REST auth expects. It mirrors services/infrastructure/internal/server/
// ed25519_auth.go exactly:
//
//   - The signed message is "<unix-ts>:<METHOD>:<path>:<hex(sha256(body))>",
//     where path is the request URL path (no host, no query) and body is the
//     exact request body bytes (an empty body hashes the empty string).
//   - The header value is "Ed25519 <pubkey>:<signature>:<unix-ts>", where pubkey
//     and signature are base64url without padding (RawURLEncoding), in that field
//     order, colon-separated.
func SignEd25519Request(req *http.Request, body []byte, pub ed25519.PublicKey, priv ed25519.PrivateKey, now time.Time) {
	tsStr := strconv.FormatInt(now.Unix(), 10)
	sum := sha256.Sum256(body)
	message := fmt.Sprintf("%s:%s:%s:%s", tsStr, req.Method, req.URL.Path, hex.EncodeToString(sum[:]))
	sig := ed25519.Sign(priv, []byte(message))

	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	req.Header.Set("Authorization", fmt.Sprintf("Ed25519 %s:%s:%s", pubB64, sigB64, tsStr))
}

// NotifyHeadBindDID posts the published record's location to a head so it can
// verify the binding, authenticated with the volunteer's device key over the
// head's Ed25519 REST scheme. headURL is the head's HTTP base URL (e.g.
// https://head.example.com); the request is signed with `now`. A non-2xx
// response is returned as an error including the response body.
func NotifyHeadBindDID(ctx context.Context, httpClient *http.Client, headURL, did, recordURI string, pub ed25519.PublicKey, priv ed25519.PrivateKey, now time.Time) error {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}

	body, err := json.Marshal(map[string]string{
		"did":        did,
		"record_uri": recordURI,
	})
	if err != nil {
		return fmt.Errorf("marshaling bind-did request: %w", err)
	}

	u := strings.TrimRight(headURL, "/") + bindDIDPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building bind-did request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	SignEd25519Request(req, body, pub, priv, now)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting bind-did to head: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("head returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
