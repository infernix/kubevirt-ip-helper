package ippool

// ntp resolution of the pool projection lives outside the dhcp allocator:
// a pool registration must never depend on dns (a blackholed resolver used
// to reject the whole reload of a network, or stall the startup gate of a
// new one), so the controller resolves the configured hostnames on its own
// and keeps the live pool as the last-good cache of the addresses. the
// informer resync drives the refresh cadence, a successful lookup replaces
// option 42 of the still-matching pool, and every failure is both logged
// and published as a metric instead of being silently skipped.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// ntpLookupIP is the resolver seam of the ntp resolution: production uses
// the process resolver, tests substitute a lookup which parks, fails or
// answers without touching the host configuration.
var ntpLookupIP = net.DefaultResolver.LookupIP

// the resolution budgets are variables so a test can shrink them: the
// cold-start budget bounds the one lookup a registration may wait for when
// the pool has no last-good addresses yet, the refresh budget bounds every
// later resolution the resync-driven refresh runs. both are far below any
// client-visible dhcp timeout, and an exhausted budget only leaves the
// last-good (or empty) set in place.
var (
	ntpColdStartBudget = 2 * time.Second
	ntpRefreshBudget   = 5 * time.Second
	ntpRefreshInterval = 5 * time.Minute
)

// errNTPNoIPv4 marks an entry which produced no ipv4 address although the
// lookup itself did not fail - an aaaa-only record or an empty answer. it
// is a distinct failure because the name may exist: silently dropping the
// entry (the pre-fix behaviour) hid a pool serving no option 42 at all.
var errNTPNoIPv4 = errors.New("no ipv4 records (AAAA-only or empty)")

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
func resolveNTP(ctx context.Context, entries []string) (addresses []net.IP, failures map[string]error) {
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

			ips, lookupErr := ntpLookupIP(ctx, "ip4", hostname)
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
					if fallback, fallbackErr := ntpLookupIP(ctx, "ip", hostname); fallbackErr == nil && len(fallback) > 0 && !hasIPv4Address(fallback) {
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

// forgetNTP drops the resolution bookkeeping of a pool which is torn down:
// a re-created pool of the same name must resolve immediately instead of
// inheriting the attempt timestamp (and the in-flight claim) of its
// predecessor.
func (c *Controller) forgetNTP(poolName string) {
	c.ntpMu.Lock()
	defer c.ntpMu.Unlock()

	delete(c.ntpInFlight, poolName)
	delete(c.ntpLastAttempt, poolName)
}

// noteNTP publishes the resolution outcome of a pool: the metric speaks for
// the served addresses and the last attempt, and every failed entry is
// logged with the pool name so an operator can grep for it. a failed entry
// keeps its previous address (when there is one): the live pool is the
// last-good cache, and a transient dns outage must not drop option 42 for
// the clients which already renew against it.
func (c *Controller) noteNTP(pool *kihv1.IPPool, addresses []net.IP, failures map[string]error) {
	if c.metrics != nil {
		c.metrics.UpdateIPPoolNTP(pool.Name, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, len(addresses), len(failures) > 0)
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

// refreshNTP resolves the configured ntp hostnames of a pool and applies
// the addresses to the live pool. it is spawned per pool after a
// registration and on every resync of the object, and it is guarded three
// ways: only a pool with hostname entries resolves at all, at most one
// refresh per pool runs at a time, and an applicable attempt is repeated at
// most once per ntpRefreshInterval. the resolution runs under the
// controller context with its own budget, so the worker and the era join
// never wait for dns. the addresses are applied only while the cache still
// carries exactly the entry list which was resolved: a reloaded spec (or a
// deleted pool) owns the served state, and a stale resolution must not
// overwrite it.
func (c *Controller) refreshNTP(pool *kihv1.IPPool) {
	if pool == nil {
		return
	}

	entries := pool.Spec.IPv4Config.NTP
	if !hasNTPHostname(entries) {
		return
	}

	c.ntpMu.Lock()
	if c.ntpInFlight == nil {
		c.ntpInFlight = make(map[string]bool)
	}
	if c.ntpLastAttempt == nil {
		c.ntpLastAttempt = make(map[string]time.Time)
	}
	if c.ntpInFlight[pool.Name] {
		c.ntpMu.Unlock()

		return
	}
	if lastAttempt, attempted := c.ntpLastAttempt[pool.Name]; attempted && time.Since(lastAttempt) < ntpRefreshInterval {
		c.ntpMu.Unlock()

		return
	}
	c.ntpInFlight[pool.Name] = true
	c.ntpMu.Unlock()

	defer func() {
		c.ntpMu.Lock()
		delete(c.ntpInFlight, pool.Name)
		c.ntpMu.Unlock()
	}()

	resolveCtx, cancel := context.WithTimeout(c.ctx, ntpRefreshBudget)
	addresses, failures := resolveNTP(resolveCtx, entries)
	cancel()

	if !c.ntpStillConfigured(pool, entries) {
		// a registration caches its installed projection only after the
		// dhcp pool exists, so a refresh which raced the cache write (or
		// a spec which changed under it) has nothing to apply to. the
		// attempt is not recorded: the next resync must retry at once
		// instead of waiting for the refresh interval
		return
	}

	c.ntpMu.Lock()
	c.ntpLastAttempt[pool.Name] = time.Now()
	c.ntpMu.Unlock()

	// a failed resolution never replaces the served set: the live pool is
	// the last-good cache, and applying an empty (or partial) answer would
	// drop option 42 for every client of the network. the metric and the
	// log line report the failure, and the next refresh retries it.
	previous, exists := c.dhcp.PoolNTP(pool.Spec.NetworkName)
	if len(failures) > 0 {
		if exists {
			addresses = previous
		}
	}

	applied := false
	if len(failures) == 0 {
		applied = c.dhcp.SetPoolNTP(pool.Spec.NetworkName, addresses)
	}

	if applied && (!exists || !sameNTPAddresses(previous, addresses)) {
		log.Infof("(ippool.refreshNTP) the ntp servers of pool %s were updated to %v", pool.Name, addresses)
	}

	c.noteNTP(pool, addresses, failures)
}

// ntpStillConfigured reports whether the pool the resolution was started
// for is still the cached configuration of its network with the very entry
// list which was resolved. a delete, a rename or a reloaded ntp section all
// make the stale resolution irrelevant, so it must not publish into the
// live pool.
func (c *Controller) ntpStillConfigured(pool *kihv1.IPPool, entries []string) bool {
	if c.cache == nil {
		return false
	}

	cached, err := c.cache.Get("pool", pool.Spec.NetworkName)
	if err != nil {
		return false
	}

	stored, ok := cached.(kihv1.IPPool)
	if !ok {
		return false
	}

	return stored.Name == pool.Name && reflect.DeepEqual(stored.Spec.IPv4Config.NTP, entries)
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
