package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
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
	mux.HandleFunc("GET /api/health", h.handleHealth)
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req vm.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := h.mgr.Create(&req)
	if err != nil {
		log.Printf("Create failed: %v", err)
		if vm.IsCapacityError(err) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, resp)
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
		"name":       st.Name,
		"cow_path":   cowPath,
		"cow_bytes":  info.Size(),
		"vm_state":   st,
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
