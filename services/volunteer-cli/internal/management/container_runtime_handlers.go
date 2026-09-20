package management

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

func handleGetContainerRuntime(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bridge.GetContainerRuntimeStatus())
	}
}

func handleSetupContainerRuntime(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			CPUs     int `json:"cpus"`
			MemoryMB int `json:"memory_mb"`
			DiskGB   int `json:"disk_gb"`
		}
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
				return
			}
		}

		accepted, err := bridge.SetupContainerRuntime(req.CPUs, req.MemoryMB, req.DiskGB)
		if err != nil {
			switch {
			case errors.Is(err, runtime.ErrNotInstalled):
				writeError(w, http.StatusConflict, "NOT_INSTALLED", err.Error())
			case errors.Is(err, runtime.ErrMachineBusy):
				writeError(w, http.StatusConflict, "MACHINE_BUSY", err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "SETUP_FAILED", err.Error())
			}
			return
		}
		if !accepted {
			writeJSON(w, map[string]string{
				"status":  "running",
				"message": "Container runtime already running",
			})
			return
		}
		writeAccepted(w, "starting", "Setting up the Podman machine; the runtime status reports the result")
	}
}

// writeAccepted answers a machine verb whose work runs on after the response
// (TB-87): 202 with the transitional status the runtime status route will
// report until the operation completes.
func writeAccepted(w http.ResponseWriter, status, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{
		"status":  status,
		"message": message,
	})
}

// handleStartContainerRuntime starts the Podman machine. 202: the start is
// under way — `podman machine start` takes 30–120 s on an Intel Mac, longer
// than any client should wait on one request — and GET /api/v1/container-
// runtime reports starting, then running or the failure (TB-87). 409 when
// there is nothing to start.
func handleStartContainerRuntime(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := bridge.StartContainerRuntime(); err != nil {
			switch {
			case errors.Is(err, runtime.ErrAlreadyRunning):
				writeError(w, http.StatusConflict, "ALREADY_RUNNING", err.Error())
			case errors.Is(err, runtime.ErrNotInitialized):
				writeError(w, http.StatusConflict, "NOT_INITIALIZED", err.Error())
			case errors.Is(err, runtime.ErrMachineBusy):
				writeError(w, http.StatusConflict, "MACHINE_BUSY", err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "START_FAILED", err.Error())
			}
			return
		}
		writeAccepted(w, "starting", "Starting the Podman machine; the runtime status reports the result")
	}
}

// handleRedetectContainerRuntime asks the daemon to probe for a container
// engine now instead of at its next periodic check (TB-59). 202: the probe is
// queued; poll GET /api/v1/container-runtime for the outcome. 409 with a
// reason when a probe is not applicable.
func handleRedetectContainerRuntime(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := bridge.RequestContainerRedetect(); err != nil {
			switch {
			case errors.Is(err, daemon.ErrContainerRuntimeRegistered):
				writeError(w, http.StatusConflict, "ALREADY_REGISTERED", err.Error())
			case errors.Is(err, daemon.ErrContainerNotTrusted):
				writeError(w, http.StatusConflict, "NOT_TRUSTED", err.Error())
			default:
				writeError(w, http.StatusConflict, "NOT_AVAILABLE", err.Error())
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{
			"status":  "checking",
			"message": "Checking for a container engine now; the runtime status reports the result",
		})
	}
}

// handleStopContainerRuntime stops the Podman machine. 202: the stop is under
// way and the status route reports stopping, then stopped (TB-87). The daemon
// leaves a machine stopped this way stopped until the volunteer starts it
// again (TB-88).
func handleStopContainerRuntime(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := bridge.StopContainerRuntime(); err != nil {
			switch {
			case errors.Is(err, runtime.ErrNotRunning):
				writeError(w, http.StatusConflict, "NOT_RUNNING", err.Error())
			case errors.Is(err, runtime.ErrMachineBusy):
				writeError(w, http.StatusConflict, "MACHINE_BUSY", err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "STOP_FAILED", err.Error())
			}
			return
		}
		writeAccepted(w, "stopping", "Stopping the Podman machine; the runtime status reports the result. Lettuce will not start it again by itself")
	}
}
