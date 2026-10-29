package ippool

// regression tests for the ntp resolution of the ippool controller. the
// defect these pin: the resolution of the configured ntp hostnames ran
// synchronously inside the dhcp registration under one aggregate budget, so
// a blackholing resolver rejected the whole pool reload (dropping the dns
// change of the same object) and stalled the startup gate, an unresolvable
// hostname silently dropped option 42, and an aaaa-only answer was dropped
// without a trace. the resolution now lives here: registration never waits
// for it, the live pool is the last-good cache, the informer resync drives
// the refresh, and every failure is logged and metered.

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// ntpSeam is a switchable lookup seam: it either parks every lookup until
// the context ends (a blackholed resolver), fails them, or answers with a
// fixed address list. a response can be pinned per lookup network ("ip4",
// "ip"), so a test can model a resolver whose ip4 answer collapses to "no
// such host" while its unfiltered answer still carries the aaaa record.
type ntpSeam struct {
	mu        sync.Mutex
	mode      string
	answers   []net.IP
	err       error
	byNetwork map[string]ntpSeamResponse
	started   atomic.Int64
	done      atomic.Int64
	lookedUp  []string
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
}

// setByNetwork pins the answer of each lookup network; the mode becomes
// "answer" unless a network entry says otherwise.
func (s *ntpSeam) setByNetwork(byNetwork map[string]ntpSeamResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mode = "answer"
	s.byNetwork = byNetwork
}

func (s *ntpSeam) lookup(ctx context.Context, network string, host string) ([]net.IP, error) {
	s.started.Add(1)
	defer s.done.Add(1)

	s.mu.Lock()
	mode := s.mode
	answers := append([]net.IP(nil), s.answers...)
	err := s.err
	response, pinned := s.byNetwork[network]
	s.lookedUp = append(s.lookedUp, network)
	s.mu.Unlock()

	switch {
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

// installNTPSeam installs the lookup seam for the duration of a test.
func installNTPSeam(t *testing.T, seam *ntpSeam) {
	t.Helper()

	old := ntpLookupIP
	ntpLookupIP = seam.lookup
	t.Cleanup(func() { ntpLookupIP = old })
}

// shrinkNTPBudgets shrinks the resolution budgets and interval so a test
// observes a timeout (and repeated refreshes) without waiting for the
// production values.
func shrinkNTPBudgets(t *testing.T, coldBudget, refreshBudget time.Duration, interval time.Duration) {
	t.Helper()

	oldCold, oldRefresh, oldInterval := ntpColdStartBudget, ntpRefreshBudget, ntpRefreshInterval
	ntpColdStartBudget, ntpRefreshBudget, ntpRefreshInterval = coldBudget, refreshBudget, interval
	t.Cleanup(func() {
		ntpColdStartBudget = oldCold
		ntpRefreshBudget = oldRefresh
		ntpRefreshInterval = oldInterval
	})
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

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	if err := controller.dhcp.AddPool(
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

	return controller, pool.DeepCopy()
}

func ntpMetricLabels(pool *kihv1.IPPool) map[string]string {
	return map[string]string{
		"ippool":  pool.Name,
		"subnet":  pool.Spec.IPv4Config.Subnet,
		"network": pool.Spec.NetworkName,
	}
}

// TestResolveNTPResolvesLiteralsAndHostnames pins the resolver contract:
// literal entries cost no lookup, hostname entries are resolved, the served
// order follows the configured entries, and a repeated address is served
// once.
func TestResolveNTPResolvesLiteralsAndHostnames(t *testing.T) {
	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("1.2.3.4")}, nil)
	installNTPSeam(t, seam)

	addresses, failures := resolveNTP(context.Background(), []string{"10.0.0.5", "ntp.example", "10.0.0.5"})

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
	installNTPSeam(t, seam)

	addresses, failures := resolveNTP(context.Background(), []string{"ntp-aaaa.example"})

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
	installNTPSeam(t, seam)

	addresses, failures := resolveNTP(context.Background(), []string{"ntp-aaaa.example"})

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
		installNTPSeam(t, seam)

		_, failures := resolveNTP(context.Background(), []string{"ntp-aaaa.example"})
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
		installNTPSeam(t, seam)

		_, failures := resolveNTP(context.Background(), []string{"ntp-aaaa.example"})
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
	installNTPSeam(t, seam)

	addresses, failures := resolveNTP(context.Background(), []string{"missing.example", "10.0.0.5"})

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
	installNTPSeam(t, seam)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	addresses, failures := resolveNTP(ctx, []string{"hanging.example", "hanging2.example"})
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
	shrinkNTPBudgets(t, 100*time.Millisecond, 100*time.Millisecond, 0)

	seam := &ntpSeam{}
	seam.set("park", nil, nil)
	installNTPSeam(t, seam)

	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	pool := testPool("pool-cold", "infra/net-cold", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-cold.example"}

	indexer := newTestIndexer()
	if err := indexer.Add(pool); err != nil {
		t.Fatalf("indexing the pool: %v", err)
	}
	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	t.Cleanup(func() { waitNTPIdle(t, controller, pool.Name) })

	started := time.Now()
	if err := controller.createOrUpdateDHCPPool(pool.DeepCopy()); err != nil {
		t.Fatalf("the cold start rejected the pool: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("the cold start took %s, want it bounded by the %s budget", elapsed, ntpColdStartBudget)
	}

	// the pool is registered and serves what the failed resolution could
	// produce: no option 42, but a live registration
	if !controller.dhcp.CheckPool(pool.Spec.NetworkName) {
		t.Fatal("the cold start did not register the pool")
	}
	if served, exists := controller.dhcp.PoolNTP(pool.Spec.NetworkName); !exists || len(served) != 0 {
		t.Fatalf("served ntp = %v (exists=%v) after the failed cold start, want none", served, exists)
	}

	// the registration's own refreshes are parked as well: settle them
	// before the resolver recovers
	waitNTPIdle(t, controller, pool.Name)

	seam.set("answer", []net.IP{net.ParseIP("10.0.0.77")}, nil)

	// the resync of an unchanged object drives the refresh
	if err := controller.handleIPPoolObjectChange(*pool, pool.DeepCopy()); err != nil {
		t.Fatalf("the unchanged update was rejected: %v", err)
	}

	deadline := time.Now().Add(shutdownWait)
	for {
		served, _ := controller.dhcp.PoolNTP(pool.Spec.NetworkName)
		if len(served) == 1 && served[0].Equal(net.ParseIP("10.0.0.77")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("option 42 was not refreshed after the resolver recovered, served = %v", served)
		}
		time.Sleep(10 * time.Millisecond)
	}

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
	shrinkNTPBudgets(t, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("10.0.0.1")}, nil)
	installNTPSeam(t, seam)

	pool := testPool("pool-lastgood", "infra/net-lastgood", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-lastgood.example"}

	controller, _ := ntpTestController(t, pool, []net.IP{net.ParseIP("10.0.0.1")})

	// a successful refresh keeps the set and reports no failure
	controller.refreshNTP(pool.DeepCopy())
	if served, _ := controller.dhcp.PoolNTP(pool.Spec.NetworkName); len(served) != 1 || !served[0].Equal(net.ParseIP("10.0.0.1")) {
		t.Fatalf("served ntp = %v after the successful refresh, want [10.0.0.1]", served)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 0 {
		t.Errorf("ntp failed metric = %v (found=%v), want 0", failed, found)
	}

	// the resolver starts failing: the served set survives the attempt
	seam.set("fail", nil, errors.New("no such host"))
	controller.refreshNTP(pool.DeepCopy())

	if served, _ := controller.dhcp.PoolNTP(pool.Spec.NetworkName); len(served) != 1 || !served[0].Equal(net.ParseIP("10.0.0.1")) {
		t.Fatalf("served ntp = %v after the failed refresh, want the last-good [10.0.0.1]", served)
	}
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
	shrinkNTPBudgets(t, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("fail", nil, errors.New("no such host"))
	installNTPSeam(t, seam)

	pool := testPool("pool-recover", "infra/net-recover", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-recover.example"}

	controller, _ := ntpTestController(t, pool, nil)

	controller.refreshNTP(pool.DeepCopy())
	if served, _ := controller.dhcp.PoolNTP(pool.Spec.NetworkName); len(served) != 0 {
		t.Fatalf("served ntp = %v after the failed refresh, want none", served)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1", failed, found)
	}

	seam.set("answer", []net.IP{net.ParseIP("10.0.0.9"), net.ParseIP("10.0.0.10")}, nil)
	controller.refreshNTP(pool.DeepCopy())

	served, _ := controller.dhcp.PoolNTP(pool.Spec.NetworkName)
	if len(served) != 2 || !served[0].Equal(net.ParseIP("10.0.0.9")) || !served[1].Equal(net.ParseIP("10.0.0.10")) {
		t.Fatalf("served ntp = %v after the recovery, want [10.0.0.9 10.0.0.10]", served)
	}
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
	shrinkNTPBudgets(t, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("2001:db8::1")}, nil)
	installNTPSeam(t, seam)

	pool := testPool("pool-aaaa", "infra/net-aaaa", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-aaaa.example"}

	controller, _ := ntpTestController(t, pool, nil)

	logs := captureNTPLogs(t, func() {
		controller.refreshNTP(pool.DeepCopy())
	})

	if !bytes.Contains([]byte(logs), []byte("no ipv4 records")) {
		t.Errorf("logs do not report the aaaa-only answer: %s", logs)
	}
	if !bytes.Contains([]byte(logs), []byte(pool.Name)) {
		t.Errorf("the aaaa-only log line does not name the pool: %s", logs)
	}
	if served, _ := controller.dhcp.PoolNTP(pool.Spec.NetworkName); len(served) != 0 {
		t.Errorf("served ntp = %v, want none", served)
	}
	if servers, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_servers", ntpMetricLabels(pool)); !found || servers != 0 {
		t.Errorf("ntp servers metric = %v (found=%v), want 0", servers, found)
	}
	if failed, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_ntp_resolution_failed", ntpMetricLabels(pool)); !found || failed != 1 {
		t.Errorf("ntp failed metric = %v (found=%v), want 1", failed, found)
	}
}

// TestRefreshIsThrottledToTheInterval pins the refresh cadence: a pool is
// re-attempted at most once per interval, and the teardown bookkeeping lets
// a re-created pool of the same name resolve at once.
func TestRefreshIsThrottledToTheInterval(t *testing.T) {
	shrinkNTPBudgets(t, time.Second, time.Second, time.Hour)

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("10.0.0.31")}, nil)
	installNTPSeam(t, seam)

	pool := testPool("pool-throttle", "infra/net-throttle", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-throttle.example"}

	controller, _ := ntpTestController(t, pool, nil)

	controller.refreshNTP(pool.DeepCopy())
	if got := seam.started.Load(); got != 1 {
		t.Fatalf("lookups = %d after the first refresh, want 1", got)
	}

	// a second resync inside the interval must not query again
	controller.refreshNTP(pool.DeepCopy())
	if got := seam.started.Load(); got != 1 {
		t.Errorf("lookups = %d after a resync inside the interval, want the first attempt only", got)
	}

	// the teardown of the pool drops the bookkeeping: a re-created pool of
	// the same name resolves immediately
	controller.forgetNTP(pool.Name)
	controller.refreshNTP(pool.DeepCopy())
	if got := seam.started.Load(); got != 2 {
		t.Errorf("lookups = %d after the bookkeeping was dropped, want 2", got)
	}
}

// TestRefreshDiscardsAStaleResolution pins the cache-equality guard: a
// resolution which finished after the pool's ntp section changed must not
// publish into the live pool, and it must not consume the refresh interval
// of the new configuration either.
func TestRefreshDiscardsAStaleResolution(t *testing.T) {
	shrinkNTPBudgets(t, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("10.0.0.41")}, nil)
	installNTPSeam(t, seam)

	stale := testPool("pool-stale", "infra/net-stale", 60)
	stale.Spec.IPv4Config.NTP = []string{"ntp-stale.example"}

	// the cached pool carries the reloaded ntp section, the refresh still
	// runs for the old entry list
	cached := stale.DeepCopy()
	cached.Spec.IPv4Config.NTP = []string{"ntp-new.example"}

	controller, _ := ntpTestController(t, cached, []net.IP{net.ParseIP("10.0.0.40")})

	controller.refreshNTP(stale.DeepCopy())

	if served, _ := controller.dhcp.PoolNTP(stale.Spec.NetworkName); len(served) != 1 || !served[0].Equal(net.ParseIP("10.0.0.40")) {
		t.Fatalf("served ntp = %v after the stale refresh, want the untouched [10.0.0.40]", served)
	}

	controller.ntpMu.Lock()
	_, attempted := controller.ntpLastAttempt[stale.Name]
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
	shrinkNTPBudgets(t, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.setByNetwork(map[string]ntpSeamResponse{
		"ip4": {err: errTestNoSuchHost},
		"ip":  {answers: []net.IP{net.ParseIP("2001:db8::1")}},
	})
	installNTPSeam(t, seam)

	pool := testPool("pool-aaaa-diag", "infra/net-aaaa-diag", 60)
	pool.Spec.IPv4Config.NTP = []string{"ntp-aaaa.example"}

	controller, _ := ntpTestController(t, pool, nil)

	logs := captureNTPLogs(t, func() {
		controller.refreshNTP(pool.DeepCopy())
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
	if served, _ := controller.dhcp.PoolNTP(pool.Spec.NetworkName); len(served) != 0 {
		t.Errorf("served ntp = %v, want none", served)
	}
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
	shrinkNTPBudgets(t, time.Second, time.Second, 0)

	seam := &ntpSeam{}
	seam.set("answer", []net.IP{net.ParseIP("10.0.0.21")}, nil)
	installNTPSeam(t, seam)

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
	t.Cleanup(func() { waitNTPIdle(t, controller, pool.Name) })

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
