package ipam

import (
	"errors"
	"testing"
)

// Tests for the excluding allocation which backs the static-ip declaration
// contract: a declared address is never handed out by a fresh dynamic
// allocation, so the declaring binding can claim it through the reclaim
// path.

func excludeTestAllocator(t *testing.T) *IPAllocator {
	t.Helper()

	a := NewIPAllocator()
	if err := a.NewSubnet("net", "192.168.99.0/24", "192.168.99.1", "192.168.99.3"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	return a
}

func TestAllocateIPExcludingSkipsDeclaredAddresses(t *testing.T) {
	a := excludeTestAllocator(t)

	// 192.168.99.1 and 192.168.99.3 are declared: the only allocatable
	// address left is the middle one, whichever order the allocator
	// iterates its bitmap in
	ip, err := a.AllocateIPExcluding("net", "ns/vm [02:00:00:00:00:01]", map[string]bool{
		"192.168.99.1": true,
		"192.168.99.3": true,
	})
	if err != nil {
		t.Fatalf("AllocateIPExcluding: %v", err)
	}
	if ip != "192.168.99.2" {
		t.Errorf("allocated ip = %q, want the undeclared 192.168.99.2", ip)
	}

	// the excluded addresses stayed free
	if _, claimed := a.IPOwnedBy("net", "ns/vm [02:00:00:00:00:01]"); !claimed {
		t.Fatal("the allocation must be a named reservation of its owner")
	}
	if got := a.Used("net"); got != 1 {
		t.Errorf("used = %d, want 1 (the excluded addresses were never taken)", got)
	}

	// the declared addresses are still allocatable for their declaring
	// binding, which claims them through the reclaim path
	for _, declared := range []string{"192.168.99.1", "192.168.99.3"} {
		if _, err := a.ReclaimIP("net", declared, "ns/vm [02:00:00:00:00:02]"); err != nil {
			t.Errorf("ReclaimIP(%s) = %v, want nil", declared, err)
		}
	}
	if got := a.Used("net"); got != 3 {
		t.Errorf("used = %d, want 3", got)
	}
}

func TestAllocateIPExcludingExhaustion(t *testing.T) {
	a := excludeTestAllocator(t)

	// every address of the pool is declared: a dynamic allocation must
	// report the pool as exhausted instead of taking a declared address
	_, err := a.AllocateIPExcluding("net", "ns/vm [02:00:00:00:00:01]", map[string]bool{
		"192.168.99.1": true,
		"192.168.99.2": true,
		"192.168.99.3": true,
	})
	if err == nil {
		t.Fatal("an all-declared pool must not hand out an address")
	}
	if got := a.Used("net"); got != 0 {
		t.Errorf("used = %d, want 0", got)
	}
}

func TestAllocateIPExcludingNilSetEqualsPlainAllocation(t *testing.T) {
	a := excludeTestAllocator(t)

	// a nil exclusion (no declaration known) keeps the plain allocation
	// contract, so the fail-soft lookup path degrades to the pre-existing
	// behavior
	ip, err := a.AllocateIPExcluding("net", "ns/vm [02:00:00:00:00:01]", nil)
	if err != nil {
		t.Fatalf("AllocateIPExcluding(nil): %v", err)
	}
	if owner, ok := a.ipam["net"].owners[ip]; !ok || owner != "ns/vm [02:00:00:00:00:01]" {
		t.Errorf("owner of %s = %q (present %v), want the named owner", ip, owner, ok)
	}

	if _, err := a.AllocateIP("net", ""); err == nil {
		t.Error("an empty owner must be rejected")
	}
	if _, err := a.AllocateIPExcluding("ghost", "ns/vm", nil); !errors.Is(err, ErrSubnetNotFound) {
		t.Errorf("unknown subnet = %v, want ErrSubnetNotFound", err)
	}
	if _, err := a.AllocateIPExcluding("net", "", map[string]bool{"192.168.99.1": true}); err == nil {
		t.Error("an empty owner must be rejected by the excluding allocation too")
	}
}
