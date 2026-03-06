package sentinel

import (
	"testing"
)

func TestPutAndSwap(t *testing.T) {
	s := NewStore()

	// Put a credential
	sv, err := s.Put("vm-1", "real-token-123", "github")
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if sv == "" {
		t.Fatal("sentinel should not be empty")
	}
	if len(sv) != 64 { // 32 bytes hex = 64 chars
		t.Fatalf("sentinel length = %d, want 64", len(sv))
	}

	// Swap should return the real token
	real, ok := s.Swap(sv)
	if !ok {
		t.Fatal("Swap returned false")
	}
	if real != "real-token-123" {
		t.Fatalf("Swap returned %q, want %q", real, "real-token-123")
	}

	// Second swap should fail (one-time use)
	_, ok = s.Swap(sv)
	if ok {
		t.Fatal("second Swap should return false")
	}
}

func TestSwapUnknown(t *testing.T) {
	s := NewStore()
	_, ok := s.Swap("nonexistent")
	if ok {
		t.Fatal("Swap of unknown sentinel should return false")
	}
}

func TestPurgeVM(t *testing.T) {
	s := NewStore()

	sv1, _ := s.Put("vm-1", "tok-1", "github")
	sv2, _ := s.Put("vm-1", "tok-2", "github")
	sv3, _ := s.Put("vm-2", "tok-3", "github")

	s.PurgeVM("vm-1")

	// vm-1 sentinels should be gone
	if _, ok := s.Swap(sv1); ok {
		t.Fatal("sv1 should be purged")
	}
	if _, ok := s.Swap(sv2); ok {
		t.Fatal("sv2 should be purged")
	}

	// vm-2 sentinel should still work
	real, ok := s.Swap(sv3)
	if !ok {
		t.Fatal("sv3 should still be valid")
	}
	if real != "tok-3" {
		t.Fatalf("sv3 returned %q, want %q", real, "tok-3")
	}
}

func TestUniqueSentinels(t *testing.T) {
	s := NewStore()
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		sv, err := s.Put("vm-1", "tok", "github")
		if err != nil {
			t.Fatalf("Put %d failed: %v", i, err)
		}
		if seen[sv] {
			t.Fatalf("duplicate sentinel at iteration %d", i)
		}
		seen[sv] = true
	}
}
