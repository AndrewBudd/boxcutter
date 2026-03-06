package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewBudd/boxcutter/node/agent/internal/vm"
)

type Handler struct {
	mgr *vm.Manager
}

func NewHandler(mgr *vm.Manager) *Handler {
	return &Handler{mgr: mgr}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/vms", h.handleCreate)
	mux.HandleFunc("GET /api/vms", h.handleList)
	mux.HandleFunc("GET /api/vms/{name}", h.handleGet)
	mux.HandleFunc("DELETE /api/vms/{name}", h.handleDestroy)
	mux.HandleFunc("POST /api/vms/{name}/stop", h.handleStop)
	mux.HandleFunc("POST /api/vms/{name}/start", h.handleStart)
	mux.HandleFunc("POST /api/vms/{name}/export", h.handleExport)
	mux.HandleFunc("POST /api/vms/{name}/import", h.handleImport)
	mux.HandleFunc("POST /api/vms/{name}/import-snapshot", h.handleImportSnapshot)
	mux.HandleFunc("POST /api/vms/{name}/migrate", h.handleMigrate)
	mux.HandleFunc("GET /api/golden/versions", h.handleGoldenVersions)
	mux.HandleFunc("GET /api/golden/{version}", h.handleGoldenCheck)
	mux.HandleFunc("POST /api/golden/build", h.handleGoldenBuild)
	mux.HandleFunc("GET /api/health", h.handleHealth)
}

// progressEvent is a NDJSON line streamed during VM creation.
type progressEvent struct {
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
	// Final result fields (only on phase="ready" or phase="error")
	Name        string `json:"name,omitempty"`
	TailscaleIP string `json:"tailscale_ip,omitempty"`
	Mark        int    `json:"mark,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Status      string `json:"status,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req vm.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	flusher, canFlush := w.(http.Flusher)

	// Set up streaming progress
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusCreated)

	req.SetProgress(func(phase, message string) {
		line, _ := json.Marshal(progressEvent{Phase: phase, Message: message})
		fmt.Fprintf(w, "%s\n", line)
		if canFlush {
			flusher.Flush()
		}
	})

	resp, err := h.mgr.Create(&req)
	if err != nil {
		log.Printf("Create failed: %v", err)
		line, _ := json.Marshal(progressEvent{Phase: "error", Error: err.Error()})
		fmt.Fprintf(w, "%s\n", line)
		if canFlush {
			flusher.Flush()
		}
		return
	}

	// Final "ready" event with full response
	line, _ := json.Marshal(progressEvent{
		Phase:       "ready",
		Message:     "VM ready",
		Name:        resp.Name,
		TailscaleIP: resp.TailscaleIP,
		Mark:        resp.Mark,
		Mode:        resp.Mode,
		Status:      resp.Status,
	})
	fmt.Fprintf(w, "%s\n", line)
	if canFlush {
		flusher.Flush()
	}
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	vms, err := h.mgr.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, vms)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/vms/")
	}
	st, status, err := h.mgr.Get(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]interface{}{
		"vm":     st,
		"status": status,
	})
}

func (h *Handler) handleDestroy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractName(r.URL.Path, "/api/vms/")
	}
	if err := h.mgr.Destroy(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractStopStartName(r.URL.Path)
	}
	if err := h.mgr.Stop(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "stopped"})
}

func (h *Handler) handleStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractStopStartName(r.URL.Path)
	}
	resp, err := h.mgr.Start(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractStopStartName(r.URL.Path)
	}

	cowPath, st, err := h.mgr.ExportVM(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	info, _ := os.Stat(cowPath)

	writeJSON(w, map[string]interface{}{
		"name":     st.Name,
		"cow_path": cowPath,
		"cow_bytes": info.Size(),
		"vm_state": st,
	})
}

func (h *Handler) handleImport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractStopStartName(r.URL.Path)
	}

	// Expect multipart: vm_state (JSON) + cow_data (binary)
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "application/json") {
		// JSON-only import: COW already transferred via rsync
		var st vm.VMState
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		st.Name = name

		resp, err := h.mgr.ImportVM(&st)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, resp)
		return
	}

	// Multipart import: COW in request body
	if err := r.ParseMultipartForm(0); err != nil {
		http.Error(w, "expected multipart form", http.StatusBadRequest)
		return
	}

	// Read VM state
	stateJSON := r.FormValue("vm_state")
	var st vm.VMState
	if err := json.Unmarshal([]byte(stateJSON), &st); err != nil {
		http.Error(w, "invalid vm_state JSON", http.StatusBadRequest)
		return
	}
	st.Name = name

	// Read COW data
	cowFile, _, err := r.FormFile("cow_data")
	if err != nil {
		http.Error(w, "missing cow_data", http.StatusBadRequest)
		return
	}
	defer cowFile.Close()

	vmDir := vm.VMDir(name)
	os.MkdirAll(vmDir, 0755)
	cowPath := filepath.Join(vmDir, "cow.img")

	dst, err := os.Create(cowPath)
	if err != nil {
		http.Error(w, "creating cow file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(dst, cowFile); err != nil {
		dst.Close()
		http.Error(w, "writing cow file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	dst.Close()

	resp, err := h.mgr.ImportVM(&st)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func (h *Handler) handleImportSnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractStopStartName(r.URL.Path)
	}

	// COW + vm.snap + vm.mem already transferred to disk via tar.
	// We just need the VM state metadata to set up TAP/fwmark/vmid.
	var st vm.VMState
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	st.Name = name

	resp, err := h.mgr.ImportSnapshot(&st)
	if err != nil {
		log.Printf("Import snapshot failed for %s: %v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func (h *Handler) handleMigrate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = extractStopStartName(r.URL.Path)
	}

	var req struct {
		TargetAddr     string `json:"target_addr"`
		TargetBridgeIP string `json:"target_bridge_ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.TargetAddr == "" || req.TargetBridgeIP == "" {
		http.Error(w, "target_addr and target_bridge_ip are required", http.StatusBadRequest)
		return
	}

	resp, err := h.mgr.MigrateVM(name, req.TargetAddr, req.TargetBridgeIP)
	if err != nil {
		log.Printf("Migration failed for %s: %v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, resp)
}

func (h *Handler) handleGoldenVersions(w http.ResponseWriter, r *http.Request) {
	versions := vm.ListGoldenVersions(h.mgr.GoldenDir())
	writeJSON(w, map[string]interface{}{
		"versions": versions,
	})
}

func (h *Handler) handleGoldenCheck(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	if version == "" {
		version = strings.TrimPrefix(r.URL.Path, "/api/golden/")
	}
	if vm.HasGoldenVersion(h.mgr.GoldenDir(), version) {
		writeJSON(w, map[string]string{"version": version, "status": "available"})
	} else {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *Handler) handleGoldenBuild(w http.ResponseWriter, r *http.Request) {
	goldenPath := h.mgr.GoldenPath()
	if _, err := os.Stat(goldenPath); err == nil {
		writeJSON(w, map[string]string{"status": "already_exists"})
		return
	}

	buildScript := filepath.Join(filepath.Dir(goldenPath), "build.sh")
	if _, err := os.Stat(buildScript); err != nil {
		http.Error(w, "build script not found", http.StatusNotFound)
		return
	}

	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	emit := func(phase, message string) {
		line, _ := json.Marshal(progressEvent{Phase: phase, Message: message})
		fmt.Fprintf(w, "%s\n", line)
		if canFlush {
			flusher.Flush()
		}
	}

	emit("building", "Building golden image...")
	log.Printf("Golden image build started")

	cmd := exec.Command("bash", buildScript)
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		emit("error", err.Error())
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		emit("building", scanner.Text())
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("Golden image build failed: %v", err)
		emit("error", fmt.Sprintf("build failed: %v", err))
		return
	}

	log.Printf("Golden image build complete")
	emit("ready", "Golden image ready")
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.mgr.Health())
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func extractName(path, prefix string) string {
	s := strings.TrimPrefix(path, prefix)
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i]
	}
	return s
}

func extractStopStartName(path string) string {
	// /api/vms/{name}/stop or /api/vms/{name}/start
	path = strings.TrimPrefix(path, "/api/vms/")
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i]
	}
	return path
}
