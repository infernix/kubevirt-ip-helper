package ippool

// ntp resolution of the pool projection lives outside the dhcp allocator:
// a pool registration must never depend on dns (a blackholed resolver used
// to reject the whole reload of a network, or stall the startup gate of a
// new one), so the controller resolves the configured hostnames on its own
// and keeps the live pool as the last-good cache of the addresses. the
// informer resync drives the refresh cadence, a successful lookup replaces
// option 42 of the still-matching pool, and every failure is both logged
// and published as a metric instead of being silently skipped.
//
// two rules make the refresh safe against the worker which publishes the
// pool concurrently:
//
//   - the publication section (Controller.publishMu) serialises every
//     publication of the served state - the dhcp pool and its cached
//     projection (a registration, a reload) and the teardown - with the
//     commit of a refresh, so a resolution can never be applied between
//     the parts of a publication. the resolution itself (the dns lookups)
//     always runs outside that section.
//   - every publication installs an ntp fence: the pool uid and a local
//     generation of the network. a refresh captures the fence when it
//     claims the pool and re-validates it (uid, generation and entry
//     list) inside the section before it publishes. a reload, a teardown
//     and a re-created pool of the same name all advance the fence, so a
//     resolution which was started for the replaced projection can never
//     write into its successor.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/types"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// the production resolution budgets and the refresh cadence. the cold-start
// budget bounds the one lookup a publication may wait for when the pool has
// no retained address yet, the refresh budget bounds every later resolution
// the resync-driven refresh runs. both are far below any client-visible dhcp
// timeout, and an exhausted budget only leaves the last-good (or empty) set
// in place. the interval throttles an unchanged pool: a resync inside it
// reuses the last attempt instead of re-resolving the same entries.
const (
	defaultNTPColdStartBudget = 2 * time.Second
	defaultNTPRefreshBudget   = 5 * time.Second
	defaultNTPRefreshInterval = 5 * time.Minute
)

// errNTPNoIPv4 marks an entry which produced no ipv4 address although the
// lookup itself did not fail - an aaaa-only record or an empty answer. it
// is a distinct failure because the name may exist: silently dropping the
// entry (the pre-fix behaviour) hid a pool serving no option 42 at all.
var errNTPNoIPv4 = errors.New("no ipv4 records (AAAA-only or empty)")

// ntpLookupFunc is the resolver seam of one resolution: it resolves one
// entry under the given lookup network ("ip4", and "ip" for the aaaa-only
// diagnosis).
type ntpLookupFunc func(ctx context.Context, network string, host string) ([]net.IP, error)

// ntpClaim is the incarnation a running refresh resolved under: the pool uid
// and the fence generation the refresh captured when it claimed the pool.
// the same value is stored in the in-flight slot, so a completion can tell
// its own claim from the claim of a successor which took the slot over.
type ntpClaim struct {
	uid        types.UID
	generation uint64
}

// ntpFence is the incarnation of the ntp projection of one network: the uid
// of the pool whose projection the publication installed, its entry list and
// the local generation. every publication of the installed projection (a
// registration, a reload) and every teardown advances the generation, so a
// refresh which captured an older one can never publish into the replaced
// state. an uninstalled fence (installed=false) means the network holds no
// registration; the generation survives the teardown, so a re-created pool of
// the same name never inherits the claims or the results of its predecessor.
type ntpFence struct {
	installed  bool
	uid        types.UID
	generation uint64
	entries    []string
}

// ntpLookup returns the resolver seam of this controller. The seam is a
// controller field instead of a package global, so a test drives its own
// controller and cannot race another test through a shared variable; a
// controller built as a bare struct literal (a unit fixture) resolves
// through the process resolver instead of panicking on a nil seam.
func (c *Controller) ntpLookup() ntpLookupFunc {
	if c.ntpLookupIP == nil {
		return net.DefaultResolver.LookupIP
	}

	return c.ntpLookupIP
}

// ntpColdStartBudget returns the budget of a cold-start resolution. a
// controller built by NewController carries the production value; a bare
// struct literal falls back to it instead of resolving under an expired
// context.
func (c *Controller) ntpColdStartBudget() time.Duration {
	if c.ntpColdStartDuration == 0 {
		return defaultNTPColdStartBudget
	}

	return c.ntpColdStartDuration
}

// ntpRefreshBudget returns the budget of one refresh resolution, with the
// same production fallback as the cold-start budget.
func (c *Controller) ntpRefreshBudget() time.Duration {
	if c.ntpRefreshDuration == 0 {
		return defaultNTPRefreshBudget
	}

	return c.ntpRefreshDuration
}

// resolveNTP normalizes the configured ntp entries into ipv4 addresses.
// literal entries are parsed directly and never cost a lookup; hostname
// entries are resolved concurrently under the one caller context, so a
// resolver which blackholes is abandoned when the context ends instead of
// holding the resolution beyond its budget. a failed ip4 lookup is
// diagnosed once (an aaaa-only name answers, it just has no ipv4 record),
// only on that failure path. the results are deduplicated and keep the
// first-occurrence order of the entries, so the served option 42 stays
// predictable, and every entry which produced no address is reported per
// entry instead of being skipped.
func resolveNTP(ctx context.Context, lookup ntpLookupFunc, entries []string) (addresses []net.IP, failures map[string]error) {
	if len(entries) == 0 {
		return nil, nil
	}

	results := make([][]net.IP, len(entries))
	errs := make([]error, len(entries))

	var wg sync.WaitGroup

	for i, entry := range entries {
		if ip := net.ParseIP(entry); ip != nil {
			if ip.To4() == nil {
				errs[i] = fmt.Errorf("the entry is not an ipv4 address: %w", errNTPNoIPv4)

				continue
			}
			results[i] = []net.IP{ip}

			continue
		}

		wg.Add(1)

		go func(i int, hostname string) {
			defer wg.Done()

			ips, lookupErr := lookup(ctx, "ip4", hostname)
			if lookupErr != nil {
				// the ip4 lookup collapses a name which does not resolve
				// and a name which only has aaaa records into the same
				// "no such host" error, so the operator could not tell a
				// typo from an ipv6-only record. only on this failure path
				// (a lookup which answered never pays for it) one more
				// lookup under the same context tells the cases apart: an
				// answer with records and no ipv4 address is the explicit
				// aaaa-only failure, everything else keeps the original
				// error. a context which already ended (the budget fired)
				// is never retried.
				if ctx.Err() == nil {
					if fallback, fallbackErr := lookup(ctx, "ip", hostname); fallbackErr == nil && len(fallback) > 0 && !hasIPv4Address(fallback) {
						errs[i] = errNTPNoIPv4

						return
					}
				}

				errs[i] = lookupErr

				return
			}

			var ipv4 []net.IP
			for _, ip := range ips {
				if ip.To4() != nil {
					ipv4 = append(ipv4, ip)
				}
			}
			if len(ipv4) == 0 {
				errs[i] = errNTPNoIPv4

				return
			}
			results[i] = ipv4
		}(i, entry)
	}

	wg.Wait()

	seen := make(map[string]bool)

	for i := range results {
		if errs[i] != nil {
			if failures == nil {
				failures = make(map[string]error)
			}
			failures[entries[i]] = errs[i]
		}

		for _, ip := range results[i] {
			key := ip.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			addresses = append(addresses, ip)
		}
	}

	return addresses, failures
}

// hasIPv4Address reports whether a lookup answer contains at least one
// ipv4 address: it separates an aaaa-only answer from a name which answers
// with no address at all.
func hasIPv4Address(ips []net.IP) bool {
	for _, ip := range ips {
		if ip.To4() != nil {
			return true
		}
	}

	return false
}

// hasNTPHostname reports whether the configured ntp entries contain an
// entry which is not a literal ipv4 address: only those entries need a
// lookup, so a pool configured with literals never runs a resolution at
// all.
func hasNTPHostname(entries []string) bool {
	for _, entry := range entries {
		if ip := net.ParseIP(entry); ip == nil || ip.To4() == nil {
			return true
		}
	}

	return false
}

// literalNTPAddresses returns the parsed ipv4 literals of the configured
// entries in their configured order.
func literalNTPAddresses(entries []string) []net.IP {
	var addresses []net.IP

	for _, entry := range entries {
		if ip := net.ParseIP(entry); ip != nil {
			if ip.To4() != nil {
				addresses = append(addresses, ip)
			}
		}
	}

	return addresses
}

// removedLiteralAddresses returns the addresses of the literal entries the
// previous installed projection carried and the current entry list no
// longer carries. a literal is configuration, not a cache: an explicit edit
// of the literal entries is authoritative, which is what lets a publication
// subtract it from the retained fallback (see retainedNTPAddresses).
//
// only a literal edit subtracts. a current entry list without a single
// literal entry configures no address of its own - it moved the whole
// projection to hostnames, from an all-literal or a mixed spec - so nothing
// is a removed literal and the previous served set stays the fallback of the
// hostname entries until one of them resolves. subtracting every previous
// literal there would empty option 42 exactly while the new hostname is
// failing, which is the state the last-good fallback exists to cover.
func removedLiteralAddresses(previous []string, current []string) []net.IP {
	if len(literalNTPAddresses(current)) == 0 {
		// a spec without a literal entry is not a literal edit: the whole
		// projection switched to hostnames, so the previous served set remains
		// the fallback until one of them resolves
		return nil
	}

	var removed []net.IP

	for _, entry := range previous {
		address := net.ParseIP(entry)
		if address == nil || address.To4() == nil {
			// a hostname carries no configured address of its own; its
			// resolved address belongs to the retained fallback
			continue
		}
		if entryConfiguresAddress(current, address) {
			continue
		}
		removed = append(removed, address)
	}

	return removed
}

// entryConfiguresAddress reports whether the entry list configures the
// address as a literal.
func entryConfiguresAddress(entries []string, address net.IP) bool {
	for _, entry := range entries {
		if ip := net.ParseIP(entry); ip != nil && ip.To4() != nil && ip.Equal(address) {
			return true
		}
	}

	return false
}

// containsNTPAddress reports whether the address list contains the address
// by value.
func containsNTPAddress(addresses []net.IP, address net.IP) bool {
	for _, candidate := range addresses {
		if candidate.Equal(address) {
			return true
		}
	}

	return false
}

// dedupeNTP drops repeated addresses while keeping the first-occurrence
// order: one server listing must not be served twice in option 42.
func dedupeNTP(ips []net.IP) []net.IP {
	if len(ips) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	deduped := make([]net.IP, 0, len(ips))

	for _, ip := range ips {
		if ip == nil {
			continue
		}
		key := ip.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, ip)
	}

	return deduped
}

// ensureNTPBookkeeping creates the bookkeeping maps on first use: a
// controller built by NewController carries them, a struct-literal fixture
// gets them with its first write. the caller holds ntpMu.
func (c *Controller) ensureNTPBookkeeping() {
	if c.ntpFences == nil {
		c.ntpFences = make(map[string]ntpFence)
	}
	if c.ntpInFlight == nil {
		c.ntpInFlight = make(map[string]ntpClaim)
	}
	if c.ntpLastAttempt == nil {
		c.ntpLastAttempt = make(map[string]time.Time)
	}
	if c.ntpFailed == nil {
		c.ntpFailed = make(map[string]bool)
	}
}

// installedNTPEntries returns the ntp entry list of the installed
// projection of the network (nil when the network holds no registration).
func (c *Controller) installedNTPEntries(networkName string) []string {
	c.ntpMu.Lock()
	defer c.ntpMu.Unlock()

	fence := c.ntpFences[networkName]

	return append([]string(nil), fence.entries...)
}

// advanceNTPFence installs a new incarnation of the ntp projection of the
// network: the uid of the pool whose projection the publication installed,
// its entry list, and a generation advanced over the previous one. it runs
// inside the publication section, so a refresh which captured the previous
// incarnation can no longer commit.
//
// the new incarnation also supersedes the claim and the attempt clock of the
// previous one: the in-flight refresh of the replaced projection is fenced by
// the generation (its commit finds a mismatch) while the slot is released
// here, so the publication's own refresh starts at once instead of waiting
// for a resolution it would discard - and the reloaded entry list is resolved
// on its own schedule instead of inheriting the throttle of the entry list it
// replaced.
func (c *Controller) advanceNTPFence(pool *kihv1.IPPool) {
	c.ntpMu.Lock()
	defer c.ntpMu.Unlock()

	c.ensureNTPBookkeeping()

	networkName := pool.Spec.NetworkName

	fence := c.ntpFences[networkName]
	fence.installed = true
	fence.generation++
	fence.uid = pool.UID
	fence.entries = append([]string(nil), pool.Spec.IPv4Config.NTP...)
	c.ntpFences[networkName] = fence

	delete(c.ntpInFlight, networkName)
	delete(c.ntpLastAttempt, networkName)
}

// forgetNTP drops the resolution bookkeeping of a network whose registration
// is torn down. the generation is advanced rather than forgotten: a
// re-created pool of the same name must resolve immediately instead of
// inheriting the attempt timestamp of its predecessor, and a refresh which
// is still in flight for the predecessor must not publish into the successor.
// the in-flight slot is released here, so the successor may claim at once;
// the predecessor's completion only clears the slot while it still holds the
// predecessor's own claim (see releaseNTPRefresh).
func (c *Controller) forgetNTP(networkName string) {
	c.ntpMu.Lock()
	defer c.ntpMu.Unlock()

	c.ensureNTPBookkeeping()

	fence := c.ntpFences[networkName]
	c.ntpFences[networkName] = ntpFence{generation: fence.generation + 1}
	delete(c.ntpInFlight, networkName)
	delete(c.ntpLastAttempt, networkName)
	delete(c.ntpFailed, networkName)
}

// ntpResolutionFailed reports the outcome of the last completed resolution
// attempt of the network.
func (c *Controller) ntpResolutionFailed(networkName string) bool {
	c.ntpMu.Lock()
	defer c.ntpMu.Unlock()

	return c.ntpFailed[networkName]
}

// setNTPResolutionFailed records the outcome of a completed resolution
// attempt of the network.
func (c *Controller) setNTPResolutionFailed(networkName string, failed bool) {
	c.ntpMu.Lock()
	defer c.ntpMu.Unlock()

	c.ensureNTPBookkeeping()

	if !failed {
		delete(c.ntpFailed, networkName)

		return
	}

	c.ntpFailed[networkName] = true
}

// noteNTP publishes the ntp state of a pool after a publication: the
// served-count gauge speaks for the addresses the installed projection puts
// into option 42, the failure gauge for the last completed resolution
// attempt, and every failed entry is logged with the pool name so an
// operator can grep for it.
//
// the two gauges follow separate rules. a publication which completed a
// resolution attempt (attempted) owns the failure state and publishes its
// outcome. a publication which only reuses the last-good addresses of a
// still configured hostname keeps the state of the last attempt, so an
// unrelated reload (a lease time or dns change) cannot clear a known failure
// without a successful lookup. and a projection which carries no hostname
// entry at all clears it, because nothing is left which could fail.
//
// a failed entry keeps its previous address (when there is one): the live
// pool is the last-good cache, and a transient dns outage must not drop
// option 42 for the clients which already renew against it.
func (c *Controller) noteNTP(pool *kihv1.IPPool, addresses []net.IP, failures map[string]error, attempted bool) {
	failed := len(failures) > 0
	switch {
	case attempted:
		c.setNTPResolutionFailed(pool.Spec.NetworkName, failed)
	case hasNTPHostname(pool.Spec.IPv4Config.NTP):
		failed = c.ntpResolutionFailed(pool.Spec.NetworkName)
	default:
		failed = false
		c.setNTPResolutionFailed(pool.Spec.NetworkName, false)
	}

	if c.metrics != nil {
		c.metrics.UpdateIPPoolNTP(pool.Name, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, len(addresses), failed)
	}

	for entry, failure := range failures {
		if errors.Is(failure, errNTPNoIPv4) {
			log.Warnf("(ippool.noteNTP) the ntp entry %s of pool %s has no ipv4 records (AAAA-only or empty), keeping the last-good addresses: %s",
				entry, pool.Name, failure.Error())

			continue
		}

		log.Errorf("(ippool.noteNTP) cannot resolve the ntp entry %s of pool %s, keeping the last-good addresses: %s",
			entry, pool.Name, failure.Error())
	}
}

// ntpProjection is the option 42 projection of one publication: the
// addresses the pool serves afterwards and, when the publication resolved
// the entries itself, its per-entry failure report.
type ntpProjection struct {
	addresses []net.IP
	failures  map[string]error
	// attempted reports whether the publication ran a resolution (the cold
	// start of a pool without a retained fallback); only then does it own the
	// failure state of the pool (see noteNTP).
	attempted bool
}

// ntpProjectionForPublication builds the addresses a publication installs
// without ever making the registration depend on dns: literal entries are
// parsed, the hostname entries are served from the retained last-good
// addresses of the live pool, and only a pool with no retained address waits
// for one cold-start resolution under its budget. it runs outside the
// publication section, so a blackholed resolver delays a publication by at
// most the cold-start budget and never holds the section a refresh commit
// needs.
func (c *Controller) ntpProjectionForPublication(pool *kihv1.IPPool) ntpProjection {
	entries := pool.Spec.IPv4Config.NTP
	if !hasNTPHostname(entries) {
		// no hostname entry: nothing is resolved, so the projection is the
		// configured literals and the failure state is cleared by noteNTP
		return ntpProjection{addresses: literalNTPAddresses(entries)}
	}

	retained := c.retainedNTPAddresses(pool)
	if len(retained) > 0 {
		// the literals are served from the spec, the hostname entries from
		// the retained set; dedupe keeps a literal which the retained set
		// already contains from being served twice
		return ntpProjection{addresses: dedupeNTP(append(literalNTPAddresses(entries), retained...))}
	}

	resolveCtx, cancel := context.WithTimeout(c.ctx, c.ntpColdStartBudget())
	defer cancel()

	addresses, failures := resolveNTP(resolveCtx, c.ntpLookup(), entries)

	return ntpProjection{addresses: addresses, failures: failures, attempted: true}
}

// retainedNTPAddresses returns the part of the served set a publication may
// keep as the fallback of its hostname entries: the addresses which are not
// the literals an explicit spec edit removed.
//
// the live pool is the last-good cache of the hostname entries, so a reload
// must not drop the addresses of a name whose answer is unavailable right
// now - a dns change of the same object would otherwise empty option 42
// until the next resync. an explicit literal edit is authoritative instead:
// when the new spec still carries a literal entry, the addresses of the
// literal entries the previous installed spec carried and the new spec no
// longer carries are subtracted from the fallback, so an old
// [10.0.0.1, broken.example] replaced by [10.0.0.2, broken.example] serves
// [10.0.0.2] while the name is unresolved instead of keeping 10.0.0.1
// forever. a new spec without any literal entry is not a literal edit: it
// moved the whole projection to hostnames, and the previous served set stays
// the fallback until one of those names resolves (see
// removedLiteralAddresses).
//
// provenance is not tracked, because an address is just an address: a
// hostname which resolved to the same address as a removed literal loses it
// too. that is fail-safe rather than sticky - a pool which still carries a
// hostname entry re-adds the address on its next refresh, and a pool without
// any hostname entry left has no resolution which could serve an address
// only a removed literal ever produced.
func (c *Controller) retainedNTPAddresses(pool *kihv1.IPPool) []net.IP {
	lastGood, _ := c.dhcp.PoolNTP(pool.Spec.NetworkName)
	if len(lastGood) == 0 {
		return nil
	}

	removed := removedLiteralAddresses(c.installedNTPEntries(pool.Spec.NetworkName), pool.Spec.IPv4Config.NTP)
	if len(removed) == 0 {
		return lastGood
	}

	retained := make([]net.IP, 0, len(lastGood))

	for _, address := range lastGood {
		if containsNTPAddress(removed, address) {
			continue
		}
		retained = append(retained, address)
	}

	return retained
}

// refreshNTP resolves the configured ntp hostnames of a pool and applies the
// addresses to the live pool. it is scheduled after the publication of a
// registration and a reload (so the fence and the cached projection are
// installed) and on every resync of an unchanged object; the resync is the
// only cadence - there is no ticker and no long-lived resolution goroutine.
//
// the refresh is guarded three ways: only a pool whose installed projection
// still carries the very entry list resolves at all (the claim reads the
// fence), at most one claim per network is live, and an applicable attempt is
// repeated at most once per interval. the resolution runs outside the
// publication section under the controller context with its own budget, so
// the worker and the era join never wait for dns, and the commit re-validates
// the claim inside the section - a reloaded spec, a teardown or a re-created
// pool owns the served state and discards a stale resolution.
func (c *Controller) refreshNTP(pool *kihv1.IPPool) {
	if pool == nil {
		return
	}

	entries := pool.Spec.IPv4Config.NTP
	if !hasNTPHostname(entries) {
		return
	}

	claim, claimed := c.claimNTPRefresh(pool.Spec.NetworkName, entries)
	if !claimed {
		return
	}

	defer c.releaseNTPRefresh(pool.Spec.NetworkName, claim)

	resolveCtx, cancel := context.WithTimeout(c.ctx, c.ntpRefreshBudget())
	addresses, failures := resolveNTP(resolveCtx, c.ntpLookup(), entries)
	cancel()

	c.commitNTPRefresh(pool, claim, entries, addresses, failures)
}

// claimNTPRefresh takes the in-flight claim of a refresh when the installed
// projection of the network still carries the entry list which is about to be
// resolved and the previous attempt is outside the throttle interval. the
// returned claim is the incarnation the resolution belongs to; a network
// without an installed projection has nothing to refresh into.
func (c *Controller) claimNTPRefresh(networkName string, entries []string) (ntpClaim, bool) {
	c.ntpMu.Lock()
	defer c.ntpMu.Unlock()

	fence, exists := c.ntpFences[networkName]
	if !exists || !fence.installed || !reflect.DeepEqual(fence.entries, entries) {
		// the publication which replaced this entry list (or removed the
		// registration) owns the served state: an in-flight resolution of the
		// old list would overwrite it
		return ntpClaim{}, false
	}

	if _, busy := c.ntpInFlight[networkName]; busy {
		return ntpClaim{}, false
	}

	if lastAttempt, attempted := c.ntpLastAttempt[networkName]; attempted && time.Since(lastAttempt) < c.ntpRefreshInterval {
		return ntpClaim{}, false
	}

	claim := ntpClaim{uid: fence.uid, generation: fence.generation}
	c.ntpInFlight[networkName] = claim

	return claim, true
}

// releaseNTPRefresh frees the in-flight slot, but only while it still holds
// the claim of this refresh: a teardown (or a publication) may have released
// the slot and a successor refresh may already occupy it, and the predecessor's
// completion must never steal that claim.
func (c *Controller) releaseNTPRefresh(networkName string, claim ntpClaim) {
	c.ntpMu.Lock()
	defer c.ntpMu.Unlock()

	if current, held := c.ntpInFlight[networkName]; held && current == claim {
		delete(c.ntpInFlight, networkName)
	}
}

// ntpClaimCurrent reports whether the incarnation the resolution was started
// for is still the installed projection of the network, with the very entry
// list which was resolved. it is called inside the publication section.
func (c *Controller) ntpClaimCurrent(networkName string, claim ntpClaim, entries []string) bool {
	c.ntpMu.Lock()
	defer c.ntpMu.Unlock()

	fence, exists := c.ntpFences[networkName]

	return exists && fence.installed && fence.uid == claim.uid && fence.generation == claim.generation &&
		reflect.DeepEqual(fence.entries, entries)
}

// commitNTPRefresh publishes a completed resolution into the live pool. it
// runs as one publication section, so the validation of the claim, the
// served-state update and the gauges are one step relative to every worker
// publication: an old resolution can never be validated against one
// projection and applied to another.
//
// a failed resolution never replaces the served set: the live pool is the
// last-good cache, and applying an empty (or partial) answer would drop
// option 42 for every client of the network. the gauges and the log line
// report the failure, and the next refresh retries it. a discarded
// resolution records no attempt either, so the next resync retries at once
// instead of waiting for the interval.
func (c *Controller) commitNTPRefresh(pool *kihv1.IPPool, claim ntpClaim, entries []string, addresses []net.IP, failures map[string]error) {
	networkName := pool.Spec.NetworkName

	c.publishMu.Lock()
	defer c.publishMu.Unlock()

	if !c.ntpClaimCurrent(networkName, claim, entries) {
		return
	}

	previous, exists := c.dhcp.PoolNTP(networkName)

	served := addresses
	if len(failures) > 0 && exists {
		served = previous
	}

	applied := false
	if len(failures) == 0 {
		applied = c.dhcp.SetPoolNTP(networkName, served)
	}

	c.ntpMu.Lock()
	c.ensureNTPBookkeeping()
	c.ntpLastAttempt[networkName] = time.Now()
	c.ntpMu.Unlock()

	if applied && (!exists || !sameNTPAddresses(previous, served)) {
		log.Infof("(ippool.refreshNTP) the ntp servers of pool %s were updated to %v", pool.Name, served)
	}

	c.noteNTP(pool, served, failures, true)
}

// sameNTPAddresses compares two address lists by value: net.IP is a byte
// slice, so the served set is equal only when every address and the order
// match.
func sameNTPAddresses(left []net.IP, right []net.IP) bool {
	if len(left) != len(right) {
		return false
	}

	for i := range left {
		if !left[i].Equal(right[i]) {
			return false
		}
	}

	return true
}
