package daemon

import (
	"context"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// The daemon re-registers on its own in two places — after a head refuses the machine's
// host id, and to re-advertise runtimes once a container engine appears — and both send
// this build's version string like the start-up registration does, so the head's record of
// the machine's build stays current.
func TestDaemon_ReRegistrationsSendClientVersion(t *testing.T) {
	const version = "0.14.0"

	t.Run("re-register after a refused host id", func(t *testing.T) {
		rc := &reRegMockClient{
			mockClient: &mockClient{},
			resp:       &lettucev1.RegisterVolunteerResponse{VolunteerId: "v", HostId: "fresh"},
		}
		head := &ServerConnection{Client: rc, Name: "server-a", Config: config.ServerConfig{GRPCAddress: "head-a:443"}, HostID: "stale"}
		d := newFetcherTestDaemon([]*ServerConnection{head})
		d.clientVersion = version

		if _, err := d.reRegisterHost(context.Background(), head); err != nil {
			t.Fatalf("reRegisterHost: %v", err)
		}
		if rc.lastReq == nil {
			t.Fatal("no re-registration sent")
		}
		if got := rc.lastReq.GetClientVersion(); got != version {
			t.Errorf("re-registration ClientVersion = %q, want %q", got, version)
		}
	})

	t.Run("re-advertise runtimes", func(t *testing.T) {
		mc, _ := tb49CountingHead()
		rc := &reRegMockClient{mockClient: mc,
			resp: &lettucev1.RegisterVolunteerResponse{VolunteerId: "vol-1", HostId: "host-1"}}
		head := &ServerConnection{Client: rc, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "host-1",
			Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
		d := newFetcherTestDaemon([]*ServerConnection{head})
		d.clientVersion = version
		d.notices = NewNoticeLog()
		d.runtimeRegistry = NewRuntimeRegistry()
		d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "wasm"})

		d.markRuntimesChanged()
		d.readvertiseIfPending(context.Background(), head)
		if rc.lastReq == nil {
			t.Fatal("no re-registration sent")
		}
		if got := rc.lastReq.GetClientVersion(); got != version {
			t.Errorf("re-advertise ClientVersion = %q, want %q", got, version)
		}
	})
}
