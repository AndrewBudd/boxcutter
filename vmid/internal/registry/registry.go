package registry

import (
	"encoding/json"
	"sync"
)

type VMRecord struct {
	VMID       string            `json:"vm_id"`
	IP         string            `json:"ip"`
	Labels     map[string]string `json:"labels,omitempty"`
	GitHubRepo string            `json:"github_repo,omitempty"`
}

type Registry struct {
	mu    sync.RWMutex
	byIP  map[string]*VMRecord
	byID  map[string]*VMRecord
}

func New() *Registry {
	return &Registry{
		byIP: make(map[string]*VMRecord),
		byID: make(map[string]*VMRecord),
	}
}

func (r *Registry) Register(rec *VMRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byIP[rec.IP] = rec
	r.byID[rec.VMID] = rec
}

func (r *Registry) Deregister(vmID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[vmID]
	if !ok {
		return false
	}
	delete(r.byIP, rec.IP)
	delete(r.byID, vmID)
	return true
}

func (r *Registry) LookupIP(ip string) (*VMRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byIP[ip]
	return rec, ok
}

func (r *Registry) LookupID(vmID string) (*VMRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[vmID]
	return rec, ok
}

func (r *Registry) List() []*VMRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	recs := make([]*VMRecord, 0, len(r.byID))
	for _, rec := range r.byID {
		recs = append(recs, rec)
	}
	return recs
}

func (r *Registry) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.List())
}
