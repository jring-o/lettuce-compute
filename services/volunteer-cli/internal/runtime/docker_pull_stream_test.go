package runtime

import (
	"strings"
	"testing"
)

// TestCheckPullStream_SurfacesInStreamError reproduces bug 1: docker/podman
// report a pull failure (here a manifest-unknown for a removed digest, the exact
// shape behind QuaXeros's "no such image" reports) as a JSON message INSIDE the
// progress stream. The old code discarded that stream, so the failed pull looked
// like success and only surfaced later as a confusing container-create error.
func TestCheckPullStream_SurfacesInStreamError(t *testing.T) {
	stream := strings.Join([]string{
		`{"status":"Trying to pull lbry.science/beyblade@sha256:fda88fd53486..."}`,
		`{"errorDetail":{"message":"manifest unknown: manifest unknown"},"error":"manifest unknown: manifest unknown"}`,
	}, "\n")

	err := checkPullStream(strings.NewReader(stream))
	if err == nil {
		t.Fatal("expected an error for an in-stream pull failure, got nil (bug 1: silently treated as success)")
	}
	if !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("error should carry the registry detail, got: %v", err)
	}
}

// TestCheckPullStream_CleanStreamOK verifies a successful pull stream returns nil.
func TestCheckPullStream_CleanStreamOK(t *testing.T) {
	stream := strings.Join([]string{
		`{"status":"Pulling from library/x"}`,
		`{"status":"Download complete"}`,
		`{"status":"Status: Downloaded newer image for library/x:latest"}`,
	}, "\n")

	if err := checkPullStream(strings.NewReader(stream)); err != nil {
		t.Errorf("clean stream should not error, got: %v", err)
	}
}
