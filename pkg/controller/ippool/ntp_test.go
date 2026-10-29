package ippool

// regression tests for the ntp resolution of the ippool controller. the
// original defect these pin: the resolution of the configured ntp hostnames
// ran synchronously inside the dhcp registration under one aggregate budget,
// so a blackholing resolver rejected the whole pool reload (dropping the dns
// change of the same object) and stalled the startup gate, an unresolvable
// hostname silently dropped option 42, and an aaaa-only answer was dropped
// without a trace. the resolution now lives here: registration never waits
// for it, the live pool is the last-good cache, the informer resync drives
// the refresh, and every failure is logged and metered.
//
// the review of the second round added the concurrency contract: the
// publication section (Controller.publishMu) and the per-network ntp fence
// (pool uid, generation and entry list) make the refresh commit atomic with
// the worker's publication and fence it against a reloaded projection, a
// teardown and a re-created pool; an explicit literal edit is subtracted
// from the retained fallback; the failure gauge follows the last completed
// resolution attempt instead of every publication; and address lists are
// deep-copied at the allocator boundary. the tests below drive each of those
// interleavings and rules through the controller-local resolution seam.

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// ntpSeam is a switchable lookup seam: it either parks every lookup until
// the context ends (a blackholed resolver), fails them, answers with a fixed
// address list, or parks each lookup until the test releases that very call
// (the interleaving mode, which lets one resolution finish while another is
// still inside the resolver). a response can be pinned per hostname or per
// lookup network ("ip4", "ip"), so a test can model a resolver whose ip4
// answer collapses to "no such host" while its unfiltered answer still
// carries the aaaa record, or a name which answers while its neighbour does
// not.
type ntpSeam struct {
	mu        sync.Mutex
	mode      string
	answers   []net.IP
	err       error
	byNetwork map[string]ntpSeamResponse
	byHost    map[string]ntpSeamResponse
	started   atomic.Int64
	done      atomic.Int64
	lookedUp  []string

	// gates holds one release channel per lookup in the "gate" mode, and
	// entered reports the index of every lookup as it parks.
	gates   []chan ntpSeamResponse
	entered chan int
}

type ntpSeamResponse struct {
	answers []net.IP
	err     error
}

func (s *ntpSeam) set(mode string, answers []net.IP, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mode = mode
	s.answers = answers
	s.err = err
	s.byNetwork = nil
	s.byHost = nil
}

// setByNetwork pins the answer of each lookup network; the mode becomes
// "answer" unless a network entry says otherwise.
func (s *ntpSeam) setByNetwork(byNetwork map[string]ntpSeamResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mode = "answer"
	s.byNetwork = byNetwork
	s.byHost = nil
}

// setByHost pins the answer of each hostname, so one entry of a pool can
// answer while another fails.
func (s *ntpSeam) setByHost(byHost map[string]ntpSeamResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mode = "answer"
	s.byHost = byHost
	s.byNetwork = nil
}

// setGated switches the seam to the interleaving mode: every lookup parks
// until the test answers that call by index (see answerLookup).
func (s *ntpSeam) setGated() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mode = "gate"
	s.gates = nil
	s.byNetwork = nil
	s.byHost = nil
	s.entered = make(chan int, 16)
}

func (s *ntpSeam) lookup(ctx context.Context, network string, host string) ([]net.IP, error) {
	s.started.Add(1)
	defer s.done.Add(1)

	s.mu.Lock()

	mode := s.mode
	answers := append([]net.IP(nil), s.answers...)
	err := s.err
	response, pinned := s.byHost[host]
	if !pinned {
		response, pinned = s.byNetwork[network]
	}
	s.lookedUp = append(s.lookedUp, network)

	var (
		index   int
		gate    chan ntpSeamResponse
		entered chan int
	)

	if mode == "gate" {
		index = len(s.gates)
		gate = make(chan ntpSeamResponse, 1)
		s.gates = append(s.gates, gate)
		entered = s.entered
	}

	s.mu.Unlock()

	switch {
	case mode == "gate":
		entered <- index

		select {
		case released := <-gate:
			return append([]net.IP(nil), released.answers...), released.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case mode == "park":
		<-ctx.Done()

		return nil, ctx.Err()
	case mode == "fail":
		return nil, err
	case pinned:
		return append([]net.IP(nil), response.answers...), response.err
	}

	return answers, nil
}

// networks returns the lookup networks the seam has been asked for, in
// order.
func (s *ntpSeam) networks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.lookedUp...)
}

// waitLookup waits until the seam has parked the lookup with the given index
// (0 is the first one since the seam was switched to the gated mode), so the
// test can order a publication against a resolution which is already inside
// the resolver.
func (s *ntpSeam) waitLookup(t *testing.T, index int) {
	t.Helper()

	for {
		select {
		case entered := <-s.entered:
			if entered != index {
				t.Fatalf("the ntp seam parked lookup %d, want lookup %d", entered, index)
			}

			return
		case <-time.After(shutdownWait):
			t.Fatalf("the gated ntp lookup %d never reached the resolver", index)
		}
	}
}

// answerLookup releases the parked lookup with the given index with the
// given answer.
func (s *ntpSeam) answerLookup(t *testing.T, index int, answers []net.IP, err error) {
	t.Helper()

	s.mu.Lock()

	if index >= len(s.gates) {
		s.mu.Unlock()
		t.Fatalf("the gated ntp lookup %d was never started", index)
	}

	gate := s.gates[index]
	s.mu.Unlock()

	gate <- ntpSeamResponse{answers: answers, err: err}
}

// shrinkNTPBudgets shrinks the resolution budgets and the refresh interval of
// one controller, so a test observes a timeout (and repeated refreshes)
// without waiting for the production values. the values live on the
// controller, so no global state has to be restored.
func shrinkNTPBudgets(t *testing.T, c *Controller, coldBudget, refreshBudget time.Duration, interval time.Duration) {
	t.Helper()

	c.ntpColdStartDuration = coldBudget
	c.ntpRefreshDuration = refreshBudget
	c.ntpRefreshInterval = interval
}

// ntpTestController builds a controller with a registered live pool and the
// pool seeded into its cache, as a serving era holds them.
func ntpTestController(t *testing.T, pool *kihv1.IPPool, served []net.IP) (*Controller, *kihv1.IPPool) {
	t.Helper()

	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	indexer := newTestIndexer()
	if err := indexer.Add(pool); err != nil {
		t.Fatalf("indexing the pool: %v", err)
	}

	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	installNTPFixture(t, controller, pool, served)

	return controller, pool.DeepCopy()
}

// installNTPFixture publishes one installed projection into the fixture: the
// dhcp pool with its served addresses, the cached object, the ntp fence of
// the projection and its gauges - exactly the served state one registration
// or reload publishes, without the registration's host machinery (nic,
// listener, ipam).
func installNTPFixture(t *testing.T, c *Controller, pool *kihv1.IPPool, served []net.IP) {
	t.Helper()

	c.publishMu.Lock()
	defer c.publishMu.Unlock()

	if err := c.dhcp.AddPool(
		context.Background(),
		pool.Spec.NetworkName,
		pool.Spec.IPv4Config.ServerIP,
		"255.255.255.0",
		pool.Spec.IPv4Config.Router,
		pool.Spec.IPv4Config.DNS,
		pool.Spec.IPv4Config.DomainName,
		pool.Spec.IPv4Config.DomainSearch,
		served,
		pool.Spec.IPv4Config.LeaseTime,
		pool.Spec.BindInterface,
	); err != nil {
		t.Fatalf("registering the live dhcp pool: %v", err)
	}
	if err := c.cache.Upsert(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	c.advanceNTPFence(pool)
	c.noteNTP(pool, served, nil, false)
}

func ntpMetricLabels(pool *kihv1.IPPool) map[string]string {
	return map[string]string{
		"ippool":  pool.Name,
		"subnet":  pool.Spec.IPv4Config.Subnet,
		"network": pool.Spec.NetworkName,
	}
}

// parseNTPAddresses parses the expected option 42 of a test into addresses.
func parseNTPAddresses(entries ...string) []net.IP {
	var addresses []net.IP

	for _, entry := range entries {
		addresses = append(addresses, net.ParseIP(entry))
	}

	return addresses
}

// assertNTPServed fails when the live pool does not serve exactly the
// expected addresses in the expected order.
func assertNTPServed(t *testing.T, c *Controller, networkName string, want ...string) {
	t.Helper()

	served, _ := c.dhcp.PoolNTP(networkName)
	if !sameNTPAddresses(served, parseNTPAddresses(want...)) {
		t.Fatalf("served option 42 of %s = %v, want exactly %v", networkName, served, want)
	}
}

// waitForNTPServed waits for the live pool to serve exactly the expected
// addresses in the expected order.
func waitForNTPServed(t *testing.T, c *Controller, networkName string, want ...string) {
	t.Helper()

	deadline := time.Now().Add(shutdownWait)
	for {
		served, _ := c.dhcp.PoolNTP(networkName)
		if sameNTPAddresses(served, parseNTPAddresses(want...)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("served option 42 of %s = %v, want %v", networkName, served, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestResolveNTPResolvesLiteralsAndHostnames pins the resolver contract:
// literal entries cost no lookup, hostname entries are resolved, the served
// order follows the configured entries, and a repeated address is served
// once.
func TestResolveNTPResolvesLiteralsAndHostnames(t *testing.T) {
	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("1.2.3.4")}, nil)

	addresses, failures := resolveNTP(context.Background(), seam.lookup, []string{"10.0.0.5", "ntp.example", "10.0.0.5"})

	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	if len(addresses) != 2 || !addresses[0].Equal(net.ParseIP("10.0.0.5")) || !addresses[1].Equal(net.ParseIP("1.2.3.4")) {
		t.Fatalf("addresses = %v, want [10.0.0.5 1.2.3.4] in configured order", addresses)
	}
	if got := seam.started.Load(); got != 1 {
		t.Errorf("lookups = %d, want only the hostname entry to be resolved", got)
	}
	if nets := seam.networks(); len(nets) != 1 || nets[0] != "ip4" {
		t.Errorf("lookups = %v, want the successful path to query ip4 only", nets)
	}
}

// TestResolveNTPReportsAAAAOnly pins the explicit no-ipv4-address failure:
// an aaaa-only (or empty) answer must be reported, not silently dropped -
// the pre-fix code filtered it out and served an option 42 which the
// operator never learned about.
func TestResolveNTPReportsAAAAOnly(t *testing.T) {
	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("2001:db8::1")}, nil)

	addresses, failures := resolveNTP(context.Background(), seam.lookup, []string{"ntp-aaaa.example"})

	if len(addresses) != 0 {
		t.Fatalf("addresses = %v, want none", addresses)
	}
	failure, reported := failures["ntp-aaaa.example"]
	if !reported {
		t.Fatalf("failures = %v, want the aaaa-only entry reported", failures)
	}
	if !errors.Is(failure, errNTPNoIPv4) {
		t.Errorf("failure = %v, want errNTPNoIPv4", failure)
	}
}

// errTestNoSuchHost models what the ip4 lookup reports for both a missing
// name and an aaaa-only name: the resolver collapses them into one error.
var errTestNoSuchHost = errors.New("lookup ntp-aaaa.example: no such host")

// TestResolveNTPDiagnosesAAAAOnlyOnLookupFailure pins the aaaa-only
// diagnosis: when the ip4 lookup fails, one unfiltered lookup under the
// same context tells an ipv6-only record apart from a missing name, so the
// operator sees the real cause instead of a bare "no such host".
func TestResolveNTPDiagnosesAAAAOnlyOnLookupFailure(t *testing.T) {
	seam := &ntpSeam{}
	seam.setByNetwork(map[string]ntpSeamResponse{
		"ip4": {err: errTestNoSuchHost},
		"ip":  {answers: []net.IP{net.ParseIP("2001:db8::1")}},
	})

	addresses, failures := resolveNTP(context.Background(), seam.lookup, []string{"ntp-aaaa.example"})

	if len(addresses) != 0 {
		t.Fatalf("addresses = %v, want none", addresses)
	}
	failure, reported := failures["ntp-aaaa.example"]
	if !reported {
		t.Fatalf("failures = %v, want the entry reported", failures)
	}
	if !errors.Is(failure, errNTPNoIPv4) {
		t.Errorf("failure = %v, want errNTPNoIPv4 instead of the raw lookup error", failure)
	}

	if nets := seam.networks(); len(nets) != 2 || nets[0] != "ip4" || nets[1] != "ip" {
		t.Errorf("lookups = %v, want exactly [ip4 ip]", nets)
	}
}

// TestResolveNTPKeepsTheLookupErrorWhenTheFallbackFails pins the negative
// side of the diagnosis: a name which really does not resolve keeps the
// original error, whether the unfiltered lookup fails as well or answers
// without any record.
func TestResolveNTPKeepsTheLookupErrorWhenTheFallbackFails(t *testing.T) {
	t.Run("the unfiltered lookup fails too", func(t *testing.T) {
		seam := &ntpSeam{}
		seam.setByNetwork(map[string]ntpSeamResponse{
			"ip4": {err: errTestNoSuchHost},
			"ip":  {err: errTestNoSuchHost},
		})

		_, failures := resolveNTP(context.Background(), seam.lookup, []string{"ntp-aaaa.example"})
		if failure, reported := failures["ntp-aaaa.example"]; !reported || !errors.Is(failure, errTestNoSuchHost) {
			t.Fatalf("failure = %v (reported=%v), want the original lookup error", failure, reported)
		}
	})

	t.Run("the unfiltered lookup answers nothing", func(t *testing.T) {
		seam := &ntpSeam{}
		seam.setByNetwork(map[string]ntpSeamResponse{
			"ip4": {err: errTestNoSuchHost},
			"ip":  {},
		})

		_, failures := resolveNTP(context.Background(), seam.lookup, []string{"ntp-aaaa.example"})
		if failure, reported := failures["ntp-aaaa.example"]; !reported || !errors.Is(failure, errTestNoSuchHost) {
			t.Fatalf("failure = %v (reported=%v), want the original lookup error", failure, reported)
		}
	})
}

// TestResolveNTPReportsLookupFailures pins the per-entry failure report for
// a name which does not resolve at all.
func TestResolveNTPReportsLookupFailures(t *testing.T) {
	seam := &ntpSeam{}
	seam.set("fail", nil, errors.New("no such host"))

	addresses, failures := resolveNTP(context.Background(), seam.lookup, []string{"missing.example", "10.0.0.5"})

	if len(addresses) != 1 || !addresses[0].Equal(net.ParseIP("10.0.0.5")) {
		t.Fatalf("addresses = %v, want the literal only", addresses)
	}
	if _, reported := failures["missing.example"]; !reported {
		t.Fatalf("failures = %v, want the failing entry reported", failures)
	}
}

// TestResolveNTPHonoursTheDeadline pins the timeout contract: a parked
// resolver is abandoned when the context ends, the failure is reported, and
// no lookup goroutine survives the return.
func TestResolveNTPHonoursTheDeadline(t *testing.T) {
	seam := &ntpSeam{}
	seam.set("park", nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	addresses, failures := resolveNTP(ctx, seam.lookup, []string{"hanging.example", "hanging2.example"})
	elapsed := time.Since(started)

	if elapsed > 2*time.Second {
		t.Errorf("resolveNTP took %s, want it bounded by the context deadline", elapsed)
	}
	if len(addresses) != 0 {
		t.Errorf("addresses = %v, want none", addresses)
	}
	if len(failures) != 2 {
		t.Fatalf("failures = %v, want both parked entries reported", failures)
	}
	if startedLookups, finishedLookups := seam.started.Load(), seam.done.Load(); startedLookups != finishedLookups {
		t.Errorf("%d lookups started but %d finished: the deadline left a lookup goroutine behind", startedLookups, finishedLookups)
	}
}

// TestColdStartBoundsTheResolutionAndRecoversOnRefresh is the cold-start
// contract: a registration against a blackholed resolver completes within
// the cold-start budget and registers the pool, and a later resync-driven
// refresh fills option 42 without a restart. the pre-fix code blocked the
// registration for its whole 30s budget and then rejected the pool, which
// stalled the startup gate.
func TestColdStartBoundsTheResolutionAndRecoversOnRefresh(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	pool := testPool("pool-cold", "infra/net-cold", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-cold.example"}

	indexer := newTestIndexer()
	if err := indexer.Add(pool); err != nil {
		t.Fatalf("indexing the pool: %v", err)
	}
	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	shrinkNTPBudgets(t, controller, 100*time.Millisecond, 100*time.Millisecond, 0)

	seam := &ntpSeam{}
	seam.set("park", nil, nil)
	controller.ntpLookupIP = seam.lookup

	started := time.Now()
	if err := controller.createOrUpdateDHCPPool(pool.DeepCopy()); err != nil {
		t.Fatalf("the cold start rejected the pool: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("the cold start took %s, want it bounded by the %s budget", elapsed, controller.ntpColdStartBudget())
	}
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	// the pool is registered and serves what the failed resolution could
	// produce: no option 42, but a live registration
	if !controller.dhcp.CheckPool(pool.Spec.NetworkName) {
		t.Fatal("the cold start did not register the pool")
	}
	assertNTPServed(t, controller, pool.Spec.NetworkName)

	seam.set("answer", []net.IP{net.ParseIP("10.0.0.77")}, nil)

	// the resync of an unchanged object drives the refresh
	if err := controller.handleIPPoolObjectChange(*pool, pool.DeepCopy()); err != nil {
		t.Fatalf("the unchanged update was rejected: %v", err)
	}

	waitForNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.77")

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 1 {
		t.Errorf("ntp servers metric = %v (found=%v), want 1", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 0 {
		t.Errorf("ntp failed metric = %v (found=%v), want 0 after the recovery", failed, found)
	}
}

// TestRefreshKeepsTheLastGoodAddressesOnFailure pins the last-good contract:
// a later resolution which fails leaves the served option 42 untouched and
// only marks the attempt as failed.
func TestRefreshKeepsTheLastGoodAddressesOnFailure(t *testing.T) {
	pool := testPool("pool-lastgood", "infra/net-lastgood", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-lastgood.example"}

	controller, cached := ntpTestController(t, pool, []net.IP{net.ParseIP("10.0.0.1")})
	shrinkNTPBudgets(t, controller, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("10.0.0.1")}, nil)
	controller.ntpLookupIP = seam.lookup

	// a successful refresh keeps the set and reports no failure
	controller.refreshNTP(cached.DeepCopy())
	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.1")

	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 0 {
		t.Errorf("ntp failed metric = %v (found=%v), want 0", failed, found)
	}

	// the resolver starts failing: the served set survives the attempt
	seam.set("fail", nil, errors.New("no such host"))
	controller.refreshNTP(cached.DeepCopy())

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.1")

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 1 {
		t.Errorf("ntp servers metric = %v (found=%v), want the last-good 1", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1", failed, found)
	}
}

// TestRefreshAppliesTheAddressesWhenDNSRecovers pins the recovery contract:
// a pool which served no option 42 because its resolution failed gets the
// addresses on the next successful refresh.
func TestRefreshAppliesTheAddressesWhenDNSRecovers(t *testing.T) {
	pool := testPool("pool-recover", "infra/net-recover", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-recover.example"}

	controller, cached := ntpTestController(t, pool, nil)
	shrinkNTPBudgets(t, controller, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("fail", nil, errors.New("no such host"))
	controller.ntpLookupIP = seam.lookup

	controller.refreshNTP(cached.DeepCopy())
	assertNTPServed(t, controller, pool.Spec.NetworkName)

	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1", failed, found)
	}

	seam.set("answer", []net.IP{net.ParseIP("10.0.0.9"), net.ParseIP("10.0.0.10")}, nil)
	controller.refreshNTP(cached.DeepCopy())

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.9", "10.0.0.10")

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 2 {
		t.Errorf("ntp servers metric = %v (found=%v), want 2", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 0 {
		t.Errorf("ntp failed metric = %v (found=%v), want 0 after the recovery", failed, found)
	}
}

// TestRefreshReportsAAAAOnlyExplicitly pins the aaaa-only contract: the
// pool serves no option 42, the warning names the pool and the entry, and
// the metrics report servers=0, failed=1. the pre-fix code dropped the
// address without a log line.
func TestRefreshReportsAAAAOnlyExplicitly(t *testing.T) {
	pool := testPool("pool-aaaa", "infra/net-aaaa", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-aaaa.example"}

	controller, cached := ntpTestController(t, pool, nil)
	shrinkNTPBudgets(t, controller, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("2001:db8::1")}, nil)
	controller.ntpLookupIP = seam.lookup

	logs := captureNTPLogs(t, func() {
		controller.refreshNTP(cached.DeepCopy())
	})

	if !bytes.Contains([]byte(logs), []byte("no ipv4 records")) {
		t.Errorf("logs do not report the aaaa-only answer: %s", logs)
	}
	if !bytes.Contains([]byte(logs), []byte(pool.Name)) {
		t.Errorf("the aaaa-only log line does not name the pool: %s", logs)
	}
	assertNTPServed(t, controller, pool.Spec.NetworkName)

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 0 {
		t.Errorf("ntp servers metric = %v (found=%v), want 0", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1", failed, found)
	}
}

// TestRefreshIsThrottledToTheInterval pins the refresh cadence: a pool is
// re-attempted at most once per interval, and the teardown bookkeeping lets a
// re-created pool of the same name resolve at once instead of inheriting the
// throttle (and the fence) of its predecessor.
func TestRefreshIsThrottledToTheInterval(t *testing.T) {
	pool := testPool("pool-throttle", "infra/net-throttle", 60)
	pool.UID = "uid-throttle"
	pool.Spec.IPv4Config.NTP = []string{"ntp-throttle.example"}

	controller, cached := ntpTestController(t, pool, nil)
	shrinkNTPBudgets(t, controller, time.Second, time.Second, time.Hour)

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("10.0.0.31")}, nil)
	controller.ntpLookupIP = seam.lookup

	controller.refreshNTP(cached.DeepCopy())
	if got := seam.started.Load(); got != 1 {
		t.Fatalf("lookups = %d after the first refresh, want 1", got)
	}

	// a second resync inside the interval must not query again
	controller.refreshNTP(cached.DeepCopy())
	if got := seam.started.Load(); got != 1 {
		t.Errorf("lookups = %d after a resync inside the interval, want the first attempt only", got)
	}

	// the teardown drops the bookkeeping and the re-created pool publishes
	// its own incarnation: it resolves immediately
	controller.forgetNTP(pool.Spec.NetworkName)

	successor := cached.DeepCopy()
	successor.UID = "uid-throttle-successor"
	installNTPFixture(t, controller, successor, nil)

	controller.refreshNTP(successor.DeepCopy())
	if got := seam.started.Load(); got != 2 {
		t.Errorf("lookups = %d after the re-created pool refreshed, want 2", got)
	}
}

// TestRefreshDiscardsAStaleResolution pins the entry-list guard of the claim:
// a refresh for an ntp section which is no longer the installed one must not
// publish into the live pool, and it must not consume the refresh interval of
// the installed configuration either.
func TestRefreshDiscardsAStaleResolution(t *testing.T) {
	pool := testPool("pool-stale", "infra/net-stale", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-stale.example"}

	// the installed projection carries the reloaded ntp section, the refresh
	// still runs for the old entry list
	cached := pool.DeepCopy()
	cached.Spec.IPv4Config.NTP = []string{"ntp-new.example"}

	controller, _ := ntpTestController(t, cached, []net.IP{net.ParseIP("10.0.0.40")})
	shrinkNTPBudgets(t, controller, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("10.0.0.41")}, nil)
	controller.ntpLookupIP = seam.lookup

	controller.refreshNTP(pool.DeepCopy())

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.40")

	if got := seam.started.Load(); got != 0 {
		t.Errorf("lookups = %d for an entry list which is not installed, want none", got)
	}

	controller.ntpMu.Lock()
	_, attempted := controller.ntpLastAttempt[pool.Spec.NetworkName]
	controller.ntpMu.Unlock()

	if attempted {
		t.Error("the stale refresh recorded an attempt, want the next resync to retry at once")
	}
}

// TestRefreshWarnsAAAAOnlyWhenOnlyTheUnfilteredLookupSeesIt pins the
// production shape of the aaaa-only case: the ip4 lookup fails with "no
// such host" (as it does for an ipv6-only name), the unfiltered lookup
// finds the record, and the pool reports the explicit no-ipv4-records
// warning plus failed=1 instead of a bare lookup error.
func TestRefreshWarnsAAAAOnlyWhenOnlyTheUnfilteredLookupSeesIt(t *testing.T) {
	pool := testPool("pool-aaaa-diag", "infra/net-aaaa-diag", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-aaaa.example"}

	controller, cached := ntpTestController(t, pool, nil)
	shrinkNTPBudgets(t, controller, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.setByNetwork(map[string]ntpSeamResponse{
		"ip4": {err: errTestNoSuchHost},
		"ip":  {answers: []net.IP{net.ParseIP("2001:db8::1")}},
	})
	controller.ntpLookupIP = seam.lookup

	logs := captureNTPLogs(t, func() {
		controller.refreshNTP(cached.DeepCopy())
	})

	if !bytes.Contains([]byte(logs), []byte("no ipv4 records")) {
		t.Errorf("logs do not report the diagnosed aaaa-only answer: %s", logs)
	}
	if bytes.Contains([]byte(logs), []byte("no such host")) {
		t.Errorf("logs still report the raw lookup error: %s", logs)
	}
	if !bytes.Contains([]byte(logs), []byte(pool.Name)) {
		t.Errorf("the warning does not name the pool: %s", logs)
	}
	assertNTPServed(t, controller, pool.Spec.NetworkName)

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 0 {
		t.Errorf("ntp servers metric = %v (found=%v), want 0", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1", failed, found)
	}
}

// TestNTPMetricsArePublishedAndRemovedOnTeardown pins the metric lifecycle:
// a successful registration publishes the served count, and the teardown of
// the pool removes both ntp series instead of leaving them behind as stale
// per-pool state.
func TestNTPMetricsArePublishedAndRemovedOnTeardown(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	pool := testPool("pool-metrics", "infra/net-metrics", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-metrics.example"}

	indexer := newTestIndexer()
	if err := indexer.Add(pool); err != nil {
		t.Fatalf("indexing the pool: %v", err)
	}
	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("10.0.0.21")}, nil)
	controller.ntpLookupIP = seam.lookup
	shrinkNTPBudgets(t, controller, time.Second, time.Second, 0)

	if err := controller.createOrUpdateDHCPPool(pool.DeepCopy()); err != nil {
		t.Fatalf("registering the pool: %v", err)
	}

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 1 {
		t.Errorf("ntp servers metric = %v (found=%v), want 1", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 0 {
		t.Errorf("ntp failed metric = %v (found=%v), want 0", failed, found)
	}

	if err := controller.cleanupIPPoolObjects(pool.DeepCopy()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if _, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); found {
		t.Error("the ntp servers series survived the teardown")
	}
	if _, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); found {
		t.Error("the ntp resolution failures series survived the teardown")
	}
}

// TestRefreshCannotOverwriteLiteralReloadAfterValidation pins the first
// interleaving of the review: a refresh of a hostname entry list is inside
// the resolver when the operator replaces that entry with a literal. the
// literal-only publication installs its own incarnation, so the resolution
// which finishes afterwards must not restore the address of the entry which
// was explicitly removed (a literal-only pool never refreshes again, so a
// leak here would persist forever).
func TestRefreshCannotOverwriteLiteralReloadAfterValidation(t *testing.T) {
	pool := testPool("pool-literal-reload", "infra/net-literal-reload", 60)
	pool.UID = "uid-literal-reload"
	pool.Spec.IPv4Config.NTP = []string{"ntp-literal-reload.example"}

	controller, cached := ntpTestController(t, pool, nil)
	shrinkNTPBudgets(t, controller, time.Second, shutdownWait, 0)

	seam := &ntpSeam{}
	seam.setGated()
	controller.ntpLookupIP = seam.lookup

	finished := make(chan struct{})

	go func() {
		defer close(finished)
		controller.refreshNTP(cached.DeepCopy())
	}()

	seam.waitLookup(t, 0)

	// the reload replaces the hostname entry with a literal
	reloaded := cached.DeepCopy()
	reloaded.Spec.IPv4Config.NTP = []string{"10.0.0.2"}
	if err := controller.handleIPPoolObjectChange(*cached, reloaded); err != nil {
		t.Fatalf("the literal reload was rejected: %v", err)
	}

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.2")

	// the resolution of the replaced entry list finishes now
	seam.answerLookup(t, 0, []net.IP{net.ParseIP("10.0.0.9")}, nil)
	<-finished

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.2")

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 1 {
		t.Errorf("ntp servers metric = %v (found=%v), want the literal projection's 1", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 0 {
		t.Errorf("ntp failed metric = %v (found=%v), want 0: the discarded resolution must not publish its outcome", failed, found)
	}
}

// TestRefreshCannotPublishIntoRecreatedPool pins the second interleaving of
// the review: the pool is deleted and re-created under the same name while a
// refresh of the predecessor is in flight. the uid of the fence tells the two
// incarnations apart, so the predecessor's addresses never enter the
// successor's served state.
func TestRefreshCannotPublishIntoRecreatedPool(t *testing.T) {
	pool := testPool("pool-recreated", "infra/net-recreated", 60)
	pool.UID = "uid-predecessor"
	pool.Spec.IPv4Config.NTP = []string{"ntp-recreated.example"}

	controller, cached := ntpTestController(t, pool, nil)
	shrinkNTPBudgets(t, controller, time.Second, shutdownWait, 0)

	seam := &ntpSeam{}
	seam.setGated()
	controller.ntpLookupIP = seam.lookup

	finished := make(chan struct{})

	go func() {
		defer close(finished)
		controller.refreshNTP(cached.DeepCopy())
	}()

	seam.waitLookup(t, 0)

	// the predecessor is torn down and a successor published under the very
	// same pool name and networkname
	if err := controller.cleanupIPPoolObjects(cached.DeepCopy()); err != nil {
		t.Fatalf("tearing the predecessor down: %v", err)
	}

	successor := cached.DeepCopy()
	successor.UID = "uid-successor"
	installNTPFixture(t, controller, successor, []net.IP{net.ParseIP("10.0.0.30")})

	seam.answerLookup(t, 0, []net.IP{net.ParseIP("10.0.0.9")}, nil)
	<-finished

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.30")

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 1 {
		t.Errorf("ntp servers metric = %v (found=%v), want the successor's 1", servers, found)
	}
}

// TestForgetNTPDoesNotReleaseAnotherRefreshClaim pins the claim bookkeeping
// of the review: the teardown of a network releases the in-flight slot, so a
// re-created pool may resolve at once - and the predecessor's completion must
// remove only its OWN claim, never the claim the successor already holds. the
// interleaving is observed twice: the successor's claim survives the
// predecessor's completion, and the successor's own resolution still commits.
func TestForgetNTPDoesNotReleaseAnotherRefreshClaim(t *testing.T) {
	pool := testPool("pool-claim", "infra/net-claim", 60)
	pool.UID = "uid-predecessor"
	pool.Spec.IPv4Config.NTP = []string{"ntp-claim.example"}

	controller, cached := ntpTestController(t, pool, nil)
	shrinkNTPBudgets(t, controller, time.Second, shutdownWait, 0)

	seam := &ntpSeam{}
	seam.setGated()
	controller.ntpLookupIP = seam.lookup

	predecessor := make(chan struct{})

	go func() {
		defer close(predecessor)
		controller.refreshNTP(cached.DeepCopy())
	}()

	seam.waitLookup(t, 0)

	// the teardown drops the bookkeeping and the re-created pool publishes
	// its own incarnation, which may claim at once
	controller.forgetNTP(pool.Spec.NetworkName)

	successor := cached.DeepCopy()
	successor.UID = "uid-successor"
	installNTPFixture(t, controller, successor, []net.IP{net.ParseIP("10.0.0.80")})

	claimed := make(chan struct{})

	go func() {
		defer close(claimed)
		controller.refreshNTP(successor.DeepCopy())
	}()

	seam.waitLookup(t, 1)

	// the predecessor completes while the successor is still resolving
	seam.answerLookup(t, 0, []net.IP{net.ParseIP("10.0.0.9")}, nil)
	<-predecessor

	controller.ntpMu.Lock()
	held, busy := controller.ntpInFlight[pool.Spec.NetworkName]
	controller.ntpMu.Unlock()

	if !busy || held.uid != "uid-successor" {
		t.Fatalf("in-flight claim = %+v (held=%v) after the predecessor completed, want the successor's claim", held, busy)
	}

	// the successor's own claim still excludes a third refresh
	before := seam.started.Load()
	controller.refreshNTP(successor.DeepCopy())
	if got := seam.started.Load(); got != before {
		t.Errorf("lookups = %d after a refresh during the successor's claim, want %d", got, before)
	}

	// and the successor's resolution still commits its addresses
	seam.answerLookup(t, 1, []net.IP{net.ParseIP("10.0.0.81")}, nil)
	<-claimed

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.81")
}

// TestFailedDNSColdStartSettlesStartupGate pins the startup contract through
// the real registration path: a pool whose only ntp entry does not resolve
// still registers (listener, allocator, cache) and settles the startup gate
// within its cold-start budget, and the address arrives later through the
// resync-driven refresh without a restart. the pre-fix code held the gate for
// the whole resolution budget and rejected the pool.
func TestFailedDNSColdStartSettlesStartupGate(t *testing.T) {
	stubNicMutation(t)

	pool := ippoolBehaviorNewTestPool("pool-ntp-gate", "infra/net-ntp-gate")
	pool.Spec.IPv4Config.NTP = []string{"ntp-gate.example"}

	restState := ippoolBehaviorNewRestState(pool)
	server := httptest.NewServer(restState.ippoolBehaviorHandler())
	t.Cleanup(server.Close)

	controller, _, dhcpAllocator, cacheAllocator, _ := ippoolBehaviorNewTestController(t, server)
	controller.scope = testNetworkScope(pool.Spec.NetworkName)
	controller.gate = newTestGate(pool.Name)
	controller.appStatus.Store(APP_INIT)
	controller.runListener = func(networkName string, nic string) error { return nil }

	seam := &ntpSeam{}
	seam.set("park", nil, nil)
	controller.ntpLookupIP = seam.lookup
	shrinkNTPBudgets(t, controller, 100*time.Millisecond, 100*time.Millisecond, 0)

	started := time.Now()
	if _, err := controller.registerIPPool(pool.DeepCopy()); err != nil {
		t.Fatalf("the registration against a blackholed resolver failed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the registration took %s, want it bounded by the cold-start budget", elapsed)
	}

	// the pool is live and the gate settles although no entry resolved
	if !dhcpAllocator.CheckPool(pool.Spec.NetworkName) {
		t.Fatal("the registration did not create the dhcp pool")
	}
	if !cacheAllocator.Check(pool) {
		t.Fatal("the registration did not cache the installed projection")
	}
	assertNTPServed(t, controller, pool.Spec.NetworkName)

	if !controller.gate.Open() {
		t.Fatalf("the startup gate did not settle: %d of %d keys settled", controller.gate.Settled(), controller.gate.Target())
	}

	// settle the registration's own refresh before the resolver recovers
	waitNTPIdle(t, controller, pool.Spec.NetworkName)

	controller.appStatus.Store(APP_RUNNING)
	seam.set("answer", []net.IP{net.ParseIP("10.10.10.60")}, nil)

	// the resync of the unchanged object recovers option 42 without a
	// restart, re-registration or pre-seeded cache
	if err := controller.handleIPPoolObjectChange(*pool, pool.DeepCopy()); err != nil {
		t.Fatalf("the unchanged update was rejected: %v", err)
	}

	waitForNTPServed(t, controller, pool.Spec.NetworkName, "10.10.10.60")
}

// TestMixedLiteralEditRemovesObsoleteLiteralDuringDNSFailure pins the literal
// subtraction rule of the review: an explicit literal edit is authoritative.
// the old projection [10.0.0.1, broken.example] serves 10.0.0.1, and the new
// projection [10.0.0.2, broken.example] must serve exactly [10.0.0.2] while
// the name is unresolved - the retained fallback keeps the hostname addresses
// but never a literal the operator removed.
func TestMixedLiteralEditRemovesObsoleteLiteralDuringDNSFailure(t *testing.T) {
	pool := testPool("pool-mixed", "infra/net-mixed", 60)
	pool.UID = "uid-mixed"
	pool.Spec.IPv4Config.NTP = []string{"10.0.0.1", "broken.example"}

	// the installed projection serves the literal; the hostname never
	// resolved, so the retained set is the literal only
	controller, cached := ntpTestController(t, pool, []net.IP{net.ParseIP("10.0.0.1")})
	shrinkNTPBudgets(t, controller, 100*time.Millisecond, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("fail", nil, errTestNoSuchHost)
	controller.ntpLookupIP = seam.lookup

	reloaded := cached.DeepCopy()
	reloaded.Spec.IPv4Config.NTP = []string{"10.0.0.2", "broken.example"}

	if err := controller.handleIPPoolObjectChange(*cached, reloaded); err != nil {
		t.Fatalf("the mixed literal edit was rejected: %v", err)
	}

	waitNTPIdle(t, controller, pool.Spec.NetworkName)

	// the exact ordered option 42: the new literal only
	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.2")

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 1 {
		t.Errorf("ntp servers metric = %v (found=%v), want 1", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1 for the unresolved entry", failed, found)
	}
}

// TestUnrelatedReloadPreservesLastResolutionFailure pins the failure rule of
// the review: the served count and the outcome of the last resolution attempt
// are separate. a reload which only changes the lease time reuses the
// retained last-good addresses without a lookup, so it must preserve the
// failing state instead of clearing the gauge without a successful attempt.
func TestUnrelatedReloadPreservesLastResolutionFailure(t *testing.T) {
	pool := testPool("pool-preserve", "infra/net-preserve", 60)
	pool.UID = "uid-preserve"
	pool.Spec.IPv4Config.NTP = []string{"ntp-preserve.example"}

	controller, cached := ntpTestController(t, pool, []net.IP{net.ParseIP("10.0.0.5")})
	shrinkNTPBudgets(t, controller, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("fail", nil, errTestNoSuchHost)
	controller.ntpLookupIP = seam.lookup

	// one completed attempt fails and records it
	controller.refreshNTP(cached.DeepCopy())

	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Fatalf("ntp failed metric = %v (found=%v), want 1 after the failed attempt", failed, found)
	}

	// the post-reload refresh is parked, so the assertions below observe the
	// publication itself
	seam.setGated()

	reloaded := cached.DeepCopy()
	reloaded.Spec.IPv4Config.LeaseTime = 120
	if err := controller.handleIPPoolObjectChange(*cached, reloaded); err != nil {
		t.Fatalf("the lease time reload was rejected: %v", err)
	}

	seam.waitLookup(t, 0)

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.5")

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 1 {
		t.Errorf("ntp servers metric = %v (found=%v), want the retained 1", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want the preserved 1: an unrelated reload must not clear a known failure", failed, found)
	}

	// the resumed attempt fails too: the state stays where it was
	seam.answerLookup(t, 0, nil, errTestNoSuchHost)
	waitNTPIdle(t, controller, pool.Spec.NetworkName)

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.5")

	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1 after the resumed failure", failed, found)
	}
}

// TestColdStartServesPartialAnswerAndRefreshIsAllOrNothing pins the two
// resolution contracts of the review: a cold start has no previous set to
// protect, so it serves the entries which answered and reports the failing
// one - while a later refresh is all-or-nothing, because replacing a served
// set with a partial answer would drop option 42 for the clients of the
// entries which failed right now.
func TestColdStartServesPartialAnswerAndRefreshIsAllOrNothing(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	pool := testPool("pool-partial", "infra/net-partial", 60)
	pool.UID = "uid-partial"
	pool.Spec.IPv4Config.NTP = []string{"ntp-ok.example", "ntp-broken.example"}

	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, nil)
	shrinkNTPBudgets(t, controller, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.setByHost(map[string]ntpSeamResponse{
		"ntp-ok.example":     {answers: []net.IP{net.ParseIP("10.0.0.11")}},
		"ntp-broken.example": {err: errTestNoSuchHost},
	})
	controller.ntpLookupIP = seam.lookup

	if err := controller.createOrUpdateDHCPPool(pool.DeepCopy()); err != nil {
		t.Fatalf("the cold start rejected the pool: %v", err)
	}

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.11")

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 1 {
		t.Errorf("ntp servers metric = %v (found=%v), want the partial 1", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1", failed, found)
	}

	// a later refresh still has one failing entry: the whole served set stays
	seam.setByHost(map[string]ntpSeamResponse{
		"ntp-ok.example":     {answers: []net.IP{net.ParseIP("10.0.0.99")}},
		"ntp-broken.example": {err: errTestNoSuchHost},
	})

	controller.refreshNTP(pool.DeepCopy())

	assertNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.11")

	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1", failed, found)
	}
}

// TestHostnameRecoveryThroughTheResyncPath pins the recovery of the very same
// hostname through the informer resync: a failed cold start leaves the pool
// live without option 42, and the unchanged object re-delivered by the resync
// re-resolves the entry and installs its address without a restart, a reload
// or a second registration.
func TestHostnameRecoveryThroughTheResyncPath(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	pool := testPool("pool-resync", "infra/net-resync", 60)
	pool.UID = "uid-resync"
	pool.Spec.IPv4Config.NTP = []string{"ntp-resync.example"}

	indexer := newTestIndexer()
	if err := indexer.Add(pool); err != nil {
		t.Fatalf("indexing the pool: %v", err)
	}
	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	shrinkNTPBudgets(t, controller, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("fail", nil, errTestNoSuchHost)
	controller.ntpLookupIP = seam.lookup

	if err := controller.createOrUpdateDHCPPool(pool.DeepCopy()); err != nil {
		t.Fatalf("the registration rejected the pool: %v", err)
	}
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	assertNTPServed(t, controller, pool.Spec.NetworkName)

	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Fatalf("ntp failed metric = %v (found=%v), want 1 after the failed cold start", failed, found)
	}

	seam.set("answer", []net.IP{net.ParseIP("10.0.0.77")}, nil)

	if err := controller.handleIPPoolObjectChange(*pool, pool.DeepCopy()); err != nil {
		t.Fatalf("the resync of the unchanged object was rejected: %v", err)
	}

	waitForNTPServed(t, controller, pool.Spec.NetworkName, "10.0.0.77")

	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 1 {
		t.Errorf("ntp servers metric = %v (found=%v), want 1", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 0 {
		t.Errorf("ntp failed metric = %v (found=%v), want 0 after the recovery", failed, found)
	}

	// the recovery is the refresh of the installed projection: the cached
	// object still carries the same (unchanged) spec
	cached, err := cacheAllocator.Get("pool", pool.Spec.NetworkName)
	if err != nil {
		t.Fatalf("the pool left the cache: %v", err)
	}
	if leaseTime := cached.(kihv1.IPPool).Spec.IPv4Config.LeaseTime; leaseTime != pool.Spec.IPv4Config.LeaseTime {
		t.Errorf("cached lease time = %d, want the unchanged %d", leaseTime, pool.Spec.IPv4Config.LeaseTime)
	}
}

// captureNTPLogs runs fn and returns the warnings and errors the standard
// logger emitted while it ran.
func captureNTPLogs(t *testing.T, fn func()) string {
	t.Helper()

	logger := log.StandardLogger()
	oldOutput := logger.Out
	oldLevel := logger.Level

	var buffer bytes.Buffer

	logger.SetOutput(&buffer)
	logger.SetLevel(log.WarnLevel)

	defer func() {
		logger.SetOutput(oldOutput)
		logger.SetLevel(oldLevel)
	}()

	fn()

	return buffer.String()
}
