package ippool

// F07 regression tests: a reloadable option update must never remove the
// last usable serving configuration before its replacement is published.
// the reload used to delete the live dhcp pool before AddPool resolved the
// replacement's ntp hostnames, so every request of that window found a
// pool-absent registry and was nacked - a valid renewal needlessly lost
// its still-valid address. the replacement is now built from addresses the
// registration already knows (the live pool is the last-good cache of the
// hostname entries), so the swap is atomic and independent of the
// resolver: neither a dns change nor a blackholed resolver delays it.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// parkedNTPLookup returns a lookup which parks until the context ends or
// the release channel closes and always fails: it mirrors a resolver which
// blackholes, so a reload which waited for it would be visibly slow instead
// of quietly broken. entered (when non-nil) is closed by the first lookup,
// so a test can wait for the background refresh to be inside the resolver.
func parkedNTPLookup(entered chan struct{}, release chan struct{}) func(ctx context.Context, network string, host string) ([]net.IP, error) {
	var once sync.Once

	return func(ctx context.Context, network string, host string) ([]net.IP, error) {
		if entered != nil {
			once.Do(func() { close(entered) })
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("the parked lookup was released without an answer")
		}
	}
}

// waitNTPIdle waits for the refresh goroutines of a network to finish, so a
// test can assert the settled state (or switch the seam) without racing a
// still running resolution.
func waitNTPIdle(t *testing.T, c *Controller, networkName string) {
	t.Helper()

	deadline := time.Now().Add(shutdownWait)
	for time.Now().Before(deadline) {
		c.ntpMu.Lock()
		_, inFlight := c.ntpInFlight[networkName]
		c.ntpMu.Unlock()

		if !inFlight {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("the ntp refresh of network %s did not settle", networkName)
}

// TestReloadWithAnNTPHostnameAppliesWithoutWaitingForTheResolver pins the
// F07 serving continuity: a reloadable update which also changes the dns
// and the ntp section of the object is published at once - the dns change
// is applied, the network keeps its last-good ntp addresses, and the
// replacement never parks in the resolver. the pre-fix code resolved the
// new ntp hostname synchronously inside the reload and a blackholed
// resolver rejected the whole reload after its 30s budget, leaving the dns
// change unapplied.
func TestReloadWithAnNTPHostnameAppliesWithoutWaitingForTheResolver(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	oldPool := testPool("pool-gap", "net-gap", 60)
	oldPool.Spec.IPv4Config.NTP = []string{"ntp-good.invalid"}
	oldPool.Spec.IPv4Config.DNS = []string{"1.1.1.1"}
	newPool := testPool("pool-gap", "net-gap", 120)
	// the new ntp entry is a hostname and the resolver blackholes: the
	// reload must still publish the dns change and keep the last-good
	// ntp address of the live pool
	newPool.Spec.IPv4Config.NTP = []string{"ntp-broken.invalid"}
	newPool.Spec.IPv4Config.DNS = []string{"8.8.8.8"}

	indexer := newTestIndexer()
	if err := indexer.Add(newPool); err != nil {
		t.Fatalf("indexing the updated pool: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	// the live registration of the old options, as a serving era holds it
	if err := controller.dhcp.AddPool(
		context.Background(),
		"net-gap",
		"192.168.1.1",
		"255.255.255.0",
		"192.168.1.1",
		[]string{"1.1.1.1"},
		"",
		nil,
		[]net.IP{net.ParseIP("192.168.1.1")},
		60,
		"test-fake-iface",
	); err != nil {
		t.Fatalf("registering the live dhcp pool: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	controller.ntpLookupIP = parkedNTPLookup(entered, release)
	// the seam lives on this controller and the parked lookup is released
	// before the test ends, so no resolution outlives the fixture
	t.Cleanup(func() {
		close(release)
		waitNTPIdle(t, controller, "net-gap")
	})

	started := time.Now()
	if err := controller.handleIPPoolObjectChange(*oldPool, newPool); err != nil {
		t.Fatalf("the reload was rejected: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("the reload took %s, want it independent of the resolver", elapsed)
	}

	// the replacement is published: the dns change is applied and the
	// network keeps serving the last-good ntp address until the refresh
	// can resolve the new hostname
	pool := controller.dhcp.GetPool("net-gap")
	if pool.LeaseTime != 120 {
		t.Errorf("serving lease time = %d after the reload, want the new 120", pool.LeaseTime)
	}
	if got := pool.DNS; len(got) != 1 || !got[0].Equal(net.ParseIP("8.8.8.8")) {
		t.Errorf("serving dns = %v after the reload, want the new [8.8.8.8]", got)
	}
	if got := pool.NTP; len(got) != 1 || !got[0].Equal(net.ParseIP("192.168.1.1")) {
		t.Errorf("serving ntp = %v after the reload, want the last-good [192.168.1.1]", got)
	}

	got, err := cacheAllocator.Get("pool", "net-gap")
	if err != nil {
		t.Fatalf("pool missing from cache: %v", err)
	}
	if leaseTime := got.(kihv1.IPPool).Spec.IPv4Config.LeaseTime; leaseTime != 120 {
		t.Errorf("cache lease time = %d after the reload, want 120", leaseTime)
	}

	// the spawned refresh owns the new hostname: it must have reached the
	// resolver, so the last-good addresses above are the ones it is about
	// to replace (and not the outcome of a silent no-op)
	select {
	case <-entered:
	case <-time.After(shutdownWait):
		t.Fatal("the background ntp refresh did not attempt the new hostname")
	}
}

// TestRejectedReplacementKeepsTheServingPool pins the F07 rejection
// continuity: a replacement whose address projection fails AddPool's own
// validation (the backstop behind the up-front admission) must leave the
// previously registered pool serving. the pre-fix code had already
// deleted the live pool when the rejection surfaced - with its delete
// error only logged - so a rejected replacement destroyed the working
// configuration.
func TestRejectedReplacementKeepsTheServingPool(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, nil)
	if err := controller.dhcp.AddPool(
		context.Background(),
		"net-gap2",
		"192.168.1.1",
		"255.255.255.0",
		"192.168.1.1",
		[]string{"1.1.1.1"},
		"",
		nil,
		nil,
		60,
		"test-fake-iface",
	); err != nil {
		t.Fatalf("registering the live dhcp pool: %v", err)
	}

	// the projection defect passes the subnet checks of the reload but is
	// rejected by AddPool's own validation
	rejected := testPool("pool-gap2", "net-gap2", 120)
	rejected.Spec.IPv4Config.DNS = []string{"dns.example.invalid"}

	if err := controller.createOrUpdateDHCPPool(rejected); err == nil {
		t.Fatal("the reload accepted an invalid dns projection, want the rejection")
	}

	// the rejected replacement must leave the serving configuration
	// untouched: the network keeps its registered pool and its options
	if !controller.dhcp.CheckPool("net-gap2") {
		t.Fatal("the rejected replacement unregistered the serving pool")
	}
	pool := controller.dhcp.GetPool("net-gap2")
	if pool.LeaseTime != 60 {
		t.Errorf("serving lease time = %d after the rejected replacement, want the old 60", pool.LeaseTime)
	}
	if got := pool.DNS; len(got) != 1 || !got[0].Equal(net.ParseIP("1.1.1.1")) {
		t.Errorf("serving dns = %v after the rejected replacement, want [1.1.1.1]", got)
	}
}
