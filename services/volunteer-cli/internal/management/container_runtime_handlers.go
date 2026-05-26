package management

import (
	"encoding/json"
	"errors"
	"net/http"

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

		if err := bridge.SetupContainerRuntime(req.CPUs, req.MemoryMB, req.DiskGB); err != nil {
			if errors.Is(err, runtime.ErrAlreadyRunning) {
				writeError(w, http.StatusConflict, "ALREADY_RUNNING", err.Error())
				return
			}
			if errors.Is(err, runtime.ErrNotInstalled) {
				writeError(w, http.StatusConflict, "NOT_INSTALLED", err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "SETUP_FAILED", err.Error())
			return
		}

		writeJSON(w, map[string]string{
			"status":  "running",
			"message": "Container runtime setup complete",
		})
	}
}

func handleStartContainerRuntime(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := bridge.StartContainerRuntime(); err != nil {
			if errors.Is(err, runtime.ErrAlreadyRunning) {
				writeError(w, http.StatusConflict, "ALREADY_RUNNING", err.Error())
				return
			}
			if errors.Is(err, runtime.ErrNotInitialized) {
				writeError(w, http.StatusConflict, "NOT_INITIALIZED", err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "START_FAILED", err.Error())
			return
		}

		writeJSON(w, map[string]string{
			"status":  "running",
			"message": "Podman machine started",
		})
	}
}

func handleStopContainerRuntime(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := bridge.StopContainerRuntime(); err != nil {
			if errors.Is(err, runtime.ErrNotRunning) {
				writeError(w, http.StatusConflict, "NOT_RUNNING", err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "STOP_FAILED", err.Error())
			return
		}

		writeJSON(w, map[string]string{
			"status":  "stopped",
			"message": "Podman machine stopped",
		})
	}
}
