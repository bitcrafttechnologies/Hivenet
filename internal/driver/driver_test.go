package driver

import (
	"strings"
	"testing"
)

// The kernel silently refuses interface names longer than IFNAMSIZ-1. Since
// link IDs are user-influenced and unbounded, the derived name must be short
// regardless of input (spec §6).
func TestVethNameFitsKernelLimit(t *testing.T) {
	ids := []string{
		"l1",
		"",
		strings.Repeat("a-very-long-link-identifier", 20),
		"link-with-unicode-名前",
	}
	for _, id := range ids {
		for end := 0; end < 2; end++ {
			name := VethName(id, end)
			if len(name) > 15 {
				t.Errorf("VethName(%.20q, %d) = %q (%d chars), exceeds IFNAMSIZ", id, end, name, len(name))
			}
			if name == "" {
				t.Errorf("VethName(%.20q, %d) returned an empty name", id, end)
			}
		}
	}
}

func TestVethNameIsStableAndDistinct(t *testing.T) {
	if VethName("l1", 0) != VethName("l1", 0) {
		t.Error("VethName must be deterministic: reconcile relies on rederiving the same name")
	}
	if VethName("l1", 0) == VethName("l1", 1) {
		t.Error("the two ends of a pair must get different names")
	}
	if VethName("l1", 0) == VethName("l2", 0) {
		t.Error("different links must get different names")
	}
}

func TestNetnsPath(t *testing.T) {
	if got, want := NetnsPath("h1"), "/var/run/netns/h1"; got != want {
		t.Errorf("NetnsPath() = %q, want %q", got, want)
	}
	if got, want := ProcNetnsPath(4242), "/proc/4242/ns/net"; got != want {
		t.Errorf("ProcNetnsPath() = %q, want %q", got, want)
	}
}
