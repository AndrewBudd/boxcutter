package sentinel

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

type entry struct {
	vmID string
	real string
	kind string
}

type Store struct {
	mu       sync.RWMutex
	bySentinel map[string]*entry
	byVM       map[string][]string // vmID → list of sentinel values
}

func NewStore() *Store {
	return &Store{
		bySentinel: make(map[string]*entry),
		byVM:       make(map[string][]string),
	}
}

// Put stores a real credential and returns a sentinel token.
func (s *Store) Put(vmID, real, kind string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	sentinel := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.bySentinel[sentinel] = &entry{vmID: vmID, real: real, kind: kind}
	s.byVM[vmID] = append(s.byVM[vmID], sentinel)
	return sentinel, nil
}

// Swap exchanges a sentinel for the real credential. One-time use.
func (s *Store) Swap(sentinel string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.bySentinel[sentinel]
	if !ok {
		return "", false
	}
	real := e.real
	delete(s.bySentinel, sentinel)
	// Remove from byVM list
	sents := s.byVM[e.vmID]
	for i, sv := range sents {
		if sv == sentinel {
			s.byVM[e.vmID] = append(sents[:i], sents[i+1:]...)
			break
		}
	}
	return real, true
}

// PurgeVM removes all sentinels for a given VM.
func (s *Store) PurgeVM(vmID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range s.byVM[vmID] {
		delete(s.bySentinel, sv)
	}
	delete(s.byVM, vmID)
}
