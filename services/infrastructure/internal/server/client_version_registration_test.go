package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Registration carries the volunteer build (RegisterVolunteerRequest.client_version) so
// the head can say which build each machine runs. These drive RegisterVolunteer as a
// direct call over registration_admission_test.go's fakes, with a stub host repo so a
// host id is issued; host_identity_integration_test.go covers the stored row end to end.

// registeredLine returns the attributes of the single "volunteer registered" record.
func registeredLine(t *testing.T, h *capturingHandler) map[string]string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var attrs map[string]string
	for _, r := range h.records {
		if r.Message != "volunteer registered" {
			continue
		}
		if attrs != nil {
			t.Fatal("more than one \"volunteer registered\" line")
		}
		attrs = map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
	}
	if attrs == nil {
		t.Fatal("no \"volunteer registered\" line")
	}
	return attrs
}

// TestRegisterVolunteer_LogsClientVersionAndHostID: both "volunteer registered" lines —
// a new account and an existing one — name the machine's issued host id and its build.
func TestRegisterVolunteer_LogsClientVersionAndHostID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
		version  string
	}{
		{"new account", false, "0.14.0"},
		{"existing account", true, "0.14.0-3-gabc1234"},
		{"a build that does not report it", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newAdmissionRecordingVolunteerRepo()
			if tc.existing {
				repo.existingVol = &volunteer.Volunteer{ID: types.NewID()}
			}
			svc := newAdmissionTestService(t, repo)
			logs := &capturingHandler{}
			svc.logger = slog.New(logs)
			svc.hostRepo = &stubHostRepo{}
			pub := newPowKeypair(t)
			req := admissionRegisterReq(pub)
			req.ClientVersion = tc.version

			resp, err := svc.RegisterVolunteer(admissionCtx(pub, ""), req)
			if err != nil {
				t.Fatalf("RegisterVolunteer: %v", err)
			}
			if resp.HostId == "" {
				t.Fatal("expected a minted host id")
			}
			attrs := registeredLine(t, logs)
			if got, ok := attrs["client_version"]; !ok || got != tc.version {
				t.Errorf("client_version on the line = %q (present %v), want %q", got, ok, tc.version)
			}
			if got := attrs["host_id"]; got != resp.HostId {
				t.Errorf("host_id on the line = %q, want the issued %q", got, resp.HostId)
			}
		})
	}
}

// TestResolveRegisteredHost_WritesClientVersionKey: the host row written on a mint and on
// an echo-refresh carries the build as the hardware_capabilities key "client_version"
// (the JSON pgx stores), and a build that does not report it writes no such key.
func TestResolveRegisteredHost_WritesClientVersionKey(t *testing.T) {
	vol := types.NewID()
	stored := func(t *testing.T, h *volunteer.Host) (string, bool) {
		t.Helper()
		raw, err := json.Marshal(h.HardwareCapabilities)
		if err != nil {
			t.Fatalf("marshal hardware: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal hardware: %v", err)
		}
		v, ok := m["client_version"]
		if !ok {
			return "", false
		}
		s, _ := v.(string)
		return s, true
	}

	for _, tc := range []struct {
		name, version string
		echo          bool
	}{
		{"mint", "0.14.0", false},
		{"echo-refresh", "0.14.1", true},
		{"mint, not reported", "", false},
		{"echo-refresh, not reported", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var written *volunteer.Host
			stub := &stubHostRepo{
				mintFn: func(h *volunteer.Host) (bool, error) { written = h; return true, nil },
				upsertFn: func(h *volunteer.Host) error {
					written = h
					return nil
				},
				getByIDFn: func(id types.ID) (*volunteer.Host, error) {
					return &volunteer.Host{ID: id, VolunteerID: vol}, nil
				},
			}
			svc := newHostIdentityService(stub, nil, HostCapPolicy{})
			hostID := ""
			if tc.echo {
				hostID = types.NewID().String()
			}
			req := hostRegisterReq(hostID)
			req.ClientVersion = tc.version

			if got := svc.resolveRegisteredHost(context.Background(), vol, req, volunteer.HardwareCapabilities{CPUCores: 4}, time.Now()); got == "" {
				t.Fatal("expected an issued host id")
			}
			if written == nil {
				t.Fatal("no host row written")
			}
			got, present := stored(t, written)
			if tc.version == "" {
				if present {
					t.Errorf("a build that does not report it wrote client_version %q; want the key absent", got)
				}
				return
			}
			if !present || got != tc.version {
				t.Errorf("stored client_version = %q (present %v), want %q", got, present, tc.version)
			}
		})
	}
}

// TestRegisterVolunteer_BoundsClientVersion: an over-long build string is refused like any
// other malformed registration field; one at the 128-byte bound is accepted.
func TestRegisterVolunteer_BoundsClientVersion(t *testing.T) {
	for _, tc := range []struct {
		n           int
		wantRefusal bool
	}{
		{128, false},
		{129, true},
	} {
		repo := newAdmissionRecordingVolunteerRepo()
		svc := newAdmissionTestService(t, repo)
		pub := newPowKeypair(t)
		req := admissionRegisterReq(pub)
		req.ClientVersion = strings.Repeat("v", tc.n)
		_, err := svc.RegisterVolunteer(admissionCtx(pub, ""), req)
		if tc.wantRefusal {
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("%d-byte client_version: err = %v, want InvalidArgument", tc.n, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%d-byte client_version refused: %v", tc.n, err)
		}
	}
}
