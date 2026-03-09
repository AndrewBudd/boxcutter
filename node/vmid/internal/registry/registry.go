package registry

import (
	"encoding/json"
	"sync"
)

type VMRecord struct {
	VMID        string            `json:"vm_id"`
	IP          string            `json:"ip"`
	Mark        int               `json:"mark"`
	Mode        string            `json:"mode"`
	Labels      map[string]string `json:"labels,omitempty"`
	GitHubRepo  string            `json:"github_repo,omitempty"`  // backwards compat
	GitHubRepos []string          `json:"github_repos,omitempty"` // all repos
}

// AllGitHubRepos returns the list of repos, falling back to single GitHubRepo.
func (r *VMRecord) AllGitHubRepos() []string {
	if len(r.GitHubRepos) > 0 {
		return r.GitHubRepos
	}
	if r.GitHubRepo != "" {
		return []string{r.GitHubRepo}
	}
	return nil
}

// AddRepo adds a repo if not already present. Returns true if added.
func (r *VMRecord) AddRepo(repo string) bool {
	for _, existing := range r.AllGitHubRepos() {
		if existing == repo {
			return false
		}
	}
	r.GitHubRepos = append(r.AllGitHubRepos(), repo)
	return true
}

// RemoveRepo removes a repo. Returns true if removed.
func (r *VMRecord) RemoveRepo(repo string) bool {
	repos := r.AllGitHubRepos()
	for i, existing := range repos {
		if existing == repo {
			r.GitHubRepos = append(repos[:i], repos[i+1:]...)
			return true
		}
	}
	return false
}

type Registry struct {
	mu     sync.RWMutex
	byIP   map[string]*VMRecord
	byID   map[string]*VMRecord
	byMark map[int]*VMRecord
}

func New() *Registry {
	return &Registry{
		byIP:   make(map[string]*VMRecord),
		byID:   make(map[string]*VMRecord),
		byMark: make(map[int]*VMRecord),
	}
}

func (r *Registry) Register(rec *VMRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byIP[rec.IP] = rec
	r.byID[rec.VMID] = rec
	if rec.Mark != 0 {
		r.byMark[rec.Mark] = rec
	}
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
	if rec.Mark != 0 {
		delete(r.byMark, rec.Mark)
	}
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

func (r *Registry) LookupMark(mark int) (*VMRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byMark[mark]
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
