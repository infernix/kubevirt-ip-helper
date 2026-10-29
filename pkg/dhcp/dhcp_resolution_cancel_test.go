package dhcp

// F08 regression tests at the new boundary: a pool registration must not
// depend on dns at all. the allocator used to resolve the ntp hostnames of
// a registration under one aggregate budget, so a blackholing resolver
// rejected the whole reload of a network - and with it the dns change of
// the same object - or stalled the startup gate of a new pool. AddPool now
// takes addresses from the caller (the ippool controller resolves and
// refreshes them), so the registration is pure state: it never waits for a
// resolver, and a canceled era still aborts before any state is taken.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestAddPoolRegistrationNeverWaitsForNTP pins the F08 boundary: the
// registration of a pool with a full ntp projection returns immediately,
// because the addresses arrive with the call and no lookup runs inside it.
// the pre-fix code resolved the entries synchronously here and a parked
// resolver either held the call for its whole budget (and then rejected it)
// or lost the entries silently.
func TestAddPoolRegistrationNeverWaitsForNTP(t *testing.T) {
	a := NewDHCPAllocator()

	ntp := make([]net.IP, 0, 200)
	for i := range 200 {
		ntp = append(ntp, net.ParseIP(fmt.Sprintf("10.0.1.%d", i%250)))
	}

	started := time.Now()
	if err := a.AddPool(
		context.Background(),
		"net-immediate",
		"192.168.0.1",
		"255.255.255.0",
		"192.168.0.254",
		nil,
		"",
		nil,
		ntp,
		60,
		"eth0",
	); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	elapsed := time.Since(started)

	if elapsed > time.Second {
		t.Errorf("the registration took %s, want it independent of any (dns) budget", elapsed)
	}
	if !a.CheckPool("net-immediate") {
		t.Fatal("the registration did not publish the pool")
	}
	served, exists := a.PoolNTP("net-immediate")
	if !exists || len(served) != len(ntp) {
		t.Fatalf("served ntp = %d entries (exists=%v), want the %d registered addresses", len(served), exists, len(ntp))
	}
	for i := range ntp {
		if !served[i].Equal(ntp[i]) {
			t.Fatalf("served ntp[%d] = %v, want %v (the configured order)", i, served[i], ntp[i])
		}
	}
}

// TestAddPoolAbortsTheRegistrationWhenTheEraIsCanceled pins the preserved
// cancellation semantics: a registration which runs on a canceled context
// publishes nothing - not a pool, not a replacement - because the era it
// would serve is already gone.
func TestAddPoolAbortsTheRegistrationWhenTheEraIsCanceled(t *testing.T) {
	t.Run("a canceled registration publishes no pool", func(t *testing.T) {
		a := NewDHCPAllocator()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := a.AddPool(ctx, "net-cancel", "192.168.0.1", "255.255.255.0", "192.168.0.254", nil, "", nil, []net.IP{net.ParseIP("10.0.0.53")}, 60, "eth0")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("AddPool on a canceled era = %v, want the propagated context.Canceled", err)
		}
		if a.CheckPool("net-cancel") {
			t.Error("the canceled registration published a pool, want no state taken")
		}
	})

	t.Run("a canceled replacement keeps the serving pool", func(t *testing.T) {
		a := newTestPooledAllocator(t)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := a.AddPool(ctx, "pool1", "192.168.0.1", "255.255.255.0", "192.168.0.254", []string{"1.1.1.1", "8.8.8.8"}, "example.com", []string{"example.com"}, []net.IP{net.ParseIP("10.0.0.54")}, 120, "eth0")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("AddPool on a canceled era = %v, want the propagated context.Canceled", err)
		}
		// the serving pool must keep its registered options: cancellation
		// is not a failed lookup whose partial result may be published
		pool := a.GetPool("pool1")
		if pool.LeaseTime != 3600 {
			t.Errorf("serving lease time = %d after the canceled replacement, want the old 3600", pool.LeaseTime)
		}
		served, _ := a.PoolNTP("pool1")
		if len(served) != 1 || !served[0].Equal(net.ParseIP("10.0.0.53")) {
			t.Errorf("serving ntp = %v after the canceled replacement, want the old [10.0.0.53]", served)
		}
	})
}
