package vm

import (
	"testing"
)

func TestTAPName_Short(t *testing.T) {
	// "tap-" + "bold-fox" = 12 chars, under 15
	got := TAPName("bold-fox")
	if got != "tap-bold-fox" {
		t.Errorf("TAPName(bold-fox) = %q, want tap-bold-fox", got)
	}
}

func TestTAPName_ExactLimit(t *testing.T) {
	// "tap-" + 11 chars = 15 (limit)
	name := "abcdefghijk" // 11 chars
	got := TAPName(name)
	if got != "tap-"+name {
		t.Errorf("TAPName(%s) = %q, want tap-%s", name, got, name)
	}
	if len(got) > 15 {
		t.Errorf("TAPName length = %d, must be <= 15", len(got))
	}
}

func TestTAPName_Long_Truncated(t *testing.T) {
	name := "very-long-vm-name-here"
	got := TAPName(name)
	if len(got) > 15 {
		t.Errorf("TAPName(%q) length = %d, must be <= 15", name, len(got))
	}
	// Should start with "tap-"
	if got[:4] != "tap-" {
		t.Errorf("TAPName should start with 'tap-', got %q", got)
	}
}

func TestTAPName_DifferentLongNames_DifferentTAPs(t *testing.T) {
	a := TAPName("very-long-name-one")
	b := TAPName("very-long-name-two")
	if a == b {
		t.Errorf("Different long names should produce different TAP names: %q == %q", a, b)
	}
}

func TestAllocateMark_Basic(t *testing.T) {
	mark := AllocateMark("test-vm", nil)
	if mark < 1 || mark > 65535 {
		t.Errorf("AllocateMark returned %d, want [1, 65535]", mark)
	}
}

func TestAllocateMark_Deterministic(t *testing.T) {
	a := AllocateMark("same-name", nil)
	b := AllocateMark("same-name", nil)
	if a != b {
		t.Errorf("Same name should produce same mark: %d != %d", a, b)
	}
}

func TestAllocateMark_CollisionAvoidance(t *testing.T) {
	// Force a collision by pre-populating the first mark
	firstMark := AllocateMark("test", nil)
	existing := map[int]bool{firstMark: true}
	secondMark := AllocateMark("test", existing)

	if secondMark == firstMark {
		t.Errorf("Should avoid collision: both returned %d", firstMark)
	}
	if secondMark < 1 || secondMark > 65535 {
		t.Errorf("Collision-resolved mark %d out of range", secondMark)
	}
}

func TestCRC32Mark_Range(t *testing.T) {
	// Test many names to ensure range is always [1, 65535]
	names := []string{
		"a", "b", "test", "bold-fox", "spicy-fox",
		"very-long-name-with-many-chars",
		"123456", "___", "a-b-c-d-e-f",
	}
	for _, name := range names {
		mark := crc32Mark(name)
		if mark < 1 || mark > 65535 {
			t.Errorf("crc32Mark(%q) = %d, want [1, 65535]", name, mark)
		}
	}
}
