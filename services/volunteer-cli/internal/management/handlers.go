package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// registerHandlers sets up all management API routes.
func registerHandlers(mux *http.ServeMux, bridge *DaemonBridge) {
	mux.HandleFunc("GET /api/v1/status", handleGetStatus(bridge))
	mux.HandleFunc("POST /api/v1/daemon/pause", handlePause(bridge))
	mux.HandleFunc("POST /api/v1/daemon/resume", handleResume(bridge))
	mux.HandleFunc("POST /api/v1/daemon/suspend-and-quit", handleSuspendAndQuit(bridge))
	mux.HandleFunc("GET /api/v1/metrics", handleGetMetrics(bridge))
	mux.HandleFunc("GET /api/v1/leafs", handleGetLeafs(bridge))
	mux.HandleFunc("POST /api/v1/leafs/attach", handleAttachLeaf(bridge))
	mux.HandleFunc("POST /api/v1/leafs/detach", handleDetachLeaf(bridge))
	mux.HandleFunc("GET /api/v1/leafs/browse", handleGetAvailableLeafsLegacy(bridge))
	mux.HandleFunc("GET /api/v1/heads", handleGetHeads(bridge))
	mux.HandleFunc("GET /api/v1/leafs/available", handleGetAvailableLeafs(bridge))
	mux.HandleFunc("GET /api/v1/history", handleGetHistory(bridge))
	mux.HandleFunc("GET /api/v1/config", handleGetConfig(bridge))
	mux.HandleFunc("PUT /api/v1/config", handleUpdateConfig(bridge))
	mux.HandleFunc("GET /api/v1/credit", handleGetCredit(bridge))
	mux.HandleFunc("POST /api/v1/identity/sign", handleSignChallenge(bridge))
	mux.HandleFunc("POST /api/v1/identity/regenerate", handleRegenerateKeypair(bridge))
	mux.HandleFunc("GET /api/v1/container-runtime", handleGetContainerRuntime(bridge))
	mux.HandleFunc("POST /api/v1/container-runtime/setup", handleSetupContainerRuntime(bridge))
	mux.HandleFunc("POST /api/v1/container-runtime/start", handleStartContainerRuntime(bridge))
	mux.HandleFunc("POST /api/v1/container-runtime/stop", handleStopContainerRuntime(bridge))
	mux.HandleFunc("POST /api/v1/tasks/{work_unit_id}/suspend", handleSuspendTask(bridge))
	mux.HandleFunc("POST /api/v1/tasks/{work_unit_id}/resume", handleResumeTask(bridge))
	mux.HandleFunc("POST /api/v1/tasks/{work_unit_id}/abort", handleAbortTask(bridge))
	mux.HandleFunc("GET /api/v1/tasks/{work_unit_id}/details", handleGetTaskDetails(bridge))
	mux.HandleFunc("GET /api/v1/results", handleListResults(bridge))
	mux.HandleFunc("GET /api/v1/results/{work_unit_id}", handleGetResult(bridge))
}

func handleGetStatus(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bridge.GetStatus())
	}
}

func handlePause(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := bridge.Pause(); err != nil {
			writeError(w, http.StatusConflict, "CONFLICT", err.Error())
			return
		}
		writeJSON(w, map[string]string{"state": "paused"})
	}
}

func handleResume(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := bridge.Resume(); err != nil {
			writeError(w, http.StatusConflict, "CONFLICT", err.Error())
			return
		}
		writeJSON(w, map[string]string{"state": "active"})
	}
}

func handleSuspendAndQuit(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Write response BEFORE calling SuspendAndQuit — it calls os.Exit(0)
		// so any code after it never runs.
		writeJSON(w, map[string]string{"state": "shutting_down"})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		bridge.SuspendAndQuit()
	}
}

func handleGetMetrics(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bridge.GetMetrics())
	}
}

func handleGetLeafs(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"leafs": bridge.GetLeafs()})
	}
}

func handleAttachLeaf(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req AttachRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
			return
		}

		if err := bridge.AttachLeaf(req); err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		writeJSON(w, map[string]string{"status": "attached"})
	}
}

func handleDetachLeaf(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req DetachRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
			return
		}

		if err := bridge.DetachLeaf(req); err != nil {
			if err == errNotFound {
				writeError(w, http.StatusNotFound, "NOT_FOUND", "Server not found")
				return
			}
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		writeJSON(w, map[string]string{"status": "detached"})
	}
}

func handleGetHeads(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"heads": bridge.GetHeads()})
	}
}

func handleGetAvailableLeafs(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"leafs": bridge.GetAvailableLeafs()})
	}
}

func handleGetAvailableLeafsLegacy(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		search := r.URL.Query().Get("search")
		area := r.URL.Query().Get("research_area")
		writeJSON(w, map[string]any{"leafs": bridge.GetAvailableLeafsLegacy(search, area)})
	}
}

func handleGetHistory(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		limit := 50
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := parseInt(l); err == nil {
				limit = n
			}
		}
		leafID := r.URL.Query().Get("leaf_id")
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")

		writeJSON(w, bridge.GetHistory(cursor, limit, leafID, from, to))
	}
}

func handleGetConfig(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bridge.GetConfig())
	}
}

func handleUpdateConfig(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var partial map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&partial); err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
			return
		}

		resp, err := bridge.UpdateConfig(partial)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		writeJSON(w, resp)
	}
}

func handleGetCredit(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bridge.GetCredit())
	}
}

func handleSignChallenge(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChallengeHex string `json:"challenge_hex"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
			return
		}
		if req.ChallengeHex == "" {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "challenge_hex is required")
			return
		}

		resp, err := bridge.SignChallenge(req.ChallengeHex)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		writeJSON(w, resp)
	}
}

func handleRegenerateKeypair(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pubKey, err := bridge.RegenerateKeypair()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		writeJSON(w, map[string]string{"public_key": pubKey})
	}
}

func parseInt(s string) (int, error) {
	return strconv.Atoi(s)
}

func handleSuspendTask(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wuID := r.PathValue("work_unit_id")
		err := bridge.SuspendTask(wuID)
		if err != nil {
			if errors.Is(err, daemon.ErrTaskNotFound) {
				writeError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
				return
			}
			if errors.Is(err, daemon.ErrTaskAlreadySuspended) {
				writeError(w, http.StatusConflict, "CONFLICT", "Task is already suspended")
				return
			}
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		writeJSON(w, map[string]string{"status": "suspended"})
	}
}

func handleResumeTask(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wuID := r.PathValue("work_unit_id")
		err := bridge.ResumeTask(wuID)
		if err != nil {
			if errors.Is(err, daemon.ErrTaskNotFound) {
				writeError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
				return
			}
			if errors.Is(err, daemon.ErrTaskNotSuspended) {
				writeError(w, http.StatusConflict, "CONFLICT", "Task is not suspended")
				return
			}
			if errors.Is(err, daemon.ErrDaemonPaused) {
				writeError(w, http.StatusConflict, "CONFLICT", "Cannot resume task while daemon is paused")
				return
			}
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		writeJSON(w, map[string]string{"status": "resumed"})
	}
}

func handleAbortTask(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wuID := r.PathValue("work_unit_id")
		err := bridge.AbortTask(wuID)
		if err != nil {
			if errors.Is(err, daemon.ErrTaskNotFound) {
				writeError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		writeJSON(w, map[string]string{"status": "aborted"})
	}
}

func handleGetTaskDetails(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wuID := r.PathValue("work_unit_id")
		detail, err := bridge.GetTaskDetails(wuID)
		if err != nil {
			if errors.Is(err, daemon.ErrTaskNotFound) {
				writeError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		writeJSON(w, detail)
	}
}

func handleListResults(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		results, err := bridge.ListResults()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		writeJSON(w, map[string]any{"results": results})
	}
}

func handleGetResult(bridge *DaemonBridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wuID := r.PathValue("work_unit_id")
		data, err := bridge.GetResultData(wuID)
		if err != nil {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Result not available locally")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}
}
