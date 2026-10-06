// Package ippoolstatus updates the persisted allocation ledger of an
// IPPool object. The vm and vmnetcfg controllers both record and remove
// their binding allocations in the pool status; the retry handling and the
// owner validation must be one implementation so the two writers cannot
// drift apart.
package ippoolstatus

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	log "github.com/sirupsen/logrus"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
)

// The conflict retry budget of updatePoolStatus. The ledger is one
// resource that every release of every vm and vmnetcfg controller writes,
// so the number of resource-version conflicts a writer must absorb grows
// with the number of concurrent writers: measured against one ledger, 2
// writers produced 1 conflict retry (2.7ms) and 8 writers 24 retries
// (10.8s), but 20 writers produced 120 retries (54.0s) and exhausted the
// former fixed 10-retry budget - 4 of the 20 releases failed with
// "cannot update status of IPPool ... after 10 retries".
//
// maxAttempts is the write-attempt budget: 30 attempts, i.e. 29 conflict
// retries. The worst case for N concurrent writers is one winner per
// attempt round (every loser re-reads the same resource version and only
// the first write of the round applies), so 30 attempts converge 30
// writers - the 20 measured writers plus 50% headroom - without
// exhausting the budget. retryBaseDelay/retryMaxDelay bound the
// exponential backoff (25ms, doubling, capped at 250ms) and full jitter
// spreads the losers of a round over the delay window so a later round
// yields several winners instead of one.
//
// Worst-case latency: 29 waits of at most retryMaxDelay (7.25s) plus 30
// GET+PUT round trips, which stays well inside the 30s client timeout of
// util.GetKubeConfig, so an exhausted budget still surfaces as an error
// rather than a wedged reconcile.
const (
	maxAttempts    = 30
	retryBaseDelay = 25 * time.Millisecond
	retryMaxDelay  = 250 * time.Millisecond
	// retryShiftCap bounds the exponential shift: 25ms << 4 = 400ms is
	// already above retryMaxDelay, so no later retry can overflow the
	// shift or the clamp.
	retryShiftCap = 4
)

// EventAdd and EventDelete are the ledger mutation tokens of UpdateStatus;
// the vm and vmnetcfg controllers alias their own event constants to them,
// so the case labels below can never drift from what the callers send.
const (
	EventAdd    = "add"
	EventDelete = "delete"
)

// UpdateStatus adds (event=ADD) or removes (event=DELETE) the allocation
// entry of one binding in the status ledger of an IPPool, converging
// against concurrent writers through resource-version conflicts and
// honoring the owner validation: an entry which the ledger records for
// another owner is never overwritten nor removed. The decision is made
// against a fresh read on every retry, so a partially applied update of a
// competing writer survives.
// Each fresh read must still name networkName, the caller's canonical
// network, before either owner convergence or a ledger mutation is allowed.
//
// The retry wait observes ctx: a canceled era (application reinit or
// shutdown) aborts the retry instead of blocking the worker for the full
// backoff.
func UpdateStatus(
	ctx context.Context,
	client *kihclientset.Clientset,
	ipam *ipam.IPAllocator,
	event string,
	vmnetcfgNamespace string,
	vmnetcfgVMName string,
	ip string,
	networkName string,
	hwAddr string,
	poolName string,
) (err error) {
	// an unknown event must never reach the persisted status - falling
	// through would rebuild the allocation map from scratch and erase
	// every live allocation entry - and it is rejected before any API
	// call, so the ledger and the request counters stay untouched
	switch event {
	case EventAdd, EventDelete:
	default:
		return fmt.Errorf("unsupported ippool status event %s for ip %s in pool %s", event, ip, poolName)
	}

	// Allocation references carry the canonical MAC spelling so add and
	// delete computations agree on owner identity across retries.
	ownerRef := util.AllocationRef(vmnetcfgNamespace, vmnetcfgVMName, hwAddr)
	return updatePoolStatus(ctx, client, networkName, poolName, func(currentPool *kihv1.IPPool) (bool, error) {
		// The ledger mutation and the counter refresh are tracked
		// separately: an idempotent delete of an address the ledger does
		// not hold (a replayed release) mutates nothing, so it only
		// needs a write while the counters still lag the live allocator.
		ledgerChanged := true
		// Allocated is published without omitempty, so a write always
		// carries a non-nil map: an empty ledger publishes {} instead of
		// null. It is built only by the branch which mutates the ledger.
		var updatedAllocated map[string]string

		switch event {
		case EventAdd:
			if existing, exists := currentPool.Status.IPv4.Allocated[ip]; exists {
				if existing != ownerRef {
					return false, fmt.Errorf("ip %s already found in IPPool status: %w", ip, util.ErrForeignOwner)
				}
				return false, nil
			}
			updatedAllocated = make(map[string]string, len(currentPool.Status.IPv4.Allocated)+1)
			for k, v := range currentPool.Status.IPv4.Allocated {
				updatedAllocated[k] = v
			}
			updatedAllocated[ip] = ownerRef
		case EventDelete:
			existing, exists := currentPool.Status.IPv4.Allocated[ip]
			if exists && existing != ownerRef {
				return false, fmt.Errorf("allocation for ip %s belongs to %s, not removing it from the %s status: %w", ip, existing, poolName, util.ErrForeignOwner)
			}
			ledgerChanged = exists
			if exists {
				updatedAllocated = make(map[string]string, len(currentPool.Status.IPv4.Allocated)-1)
				for k, v := range currentPool.Status.IPv4.Allocated {
					if k != ip {
						updatedAllocated[k] = v
					}
				}
			}
		}

		counterChanged := false
		// the counters describe the serving state of the pool: they are
		// only recomputed from the in-memory allocator while the network
		// is registered in it. a pool which exists without a registration
		// (an unregistrable spec, or a registration blocked by the very
		// record this write removes - F04) keeps its persisted counters:
		// Used and Available of an unknown network report zero, so
		// recomputing them would corrupt the durable status of a pool
		// whose ledger still holds live entries
		if ipam.HasSubnet(networkName) {
			used, available, _ := ipam.UsageCounts(networkName)
			if currentPool.Status.IPv4.Used != used || currentPool.Status.IPv4.Available != available {
				currentPool.Status.IPv4.Used = used
				currentPool.Status.IPv4.Available = available
				counterChanged = true
			}
		}

		// Nothing to persist: the address was already absent from the
		// ledger and the counters already match the live allocator, so
		// the status write would only bump LastUpdate.
		if !ledgerChanged && !counterChanged {
			return false, nil
		}
		if updatedAllocated == nil {
			// a counter-only write (an absent ledger entry whose counters
			// lagged) still republishes the ledger as a non-nil map
			updatedAllocated = make(map[string]string, len(currentPool.Status.IPv4.Allocated))
			for k, v := range currentPool.Status.IPv4.Allocated {
				updatedAllocated[k] = v
			}
		}
		currentPool.Status.IPv4.Allocated = updatedAllocated
		return true, nil
	})
}

// UpdateAccounting refreshes durable usage after local claims have been
// released, without inventing a ledger mutation. A missing local subnet
// cannot establish usage and must not be published as an empty pool.
func UpdateAccounting(ctx context.Context, client *kihclientset.Clientset, ipam *ipam.IPAllocator, networkName, poolName string) error {
	return updatePoolStatus(ctx, client, networkName, poolName, func(currentPool *kihv1.IPPool) (bool, error) {
		used, available, exists := ipam.UsageCounts(networkName)
		if !exists {
			return false, fmt.Errorf("cannot update accounting of IPPool %s: network %q is not registered in the local allocator", poolName, networkName)
		}
		if currentPool.Status.IPv4.Used == used && currentPool.Status.IPv4.Available == available {
			return false, nil
		}
		currentPool.Status.IPv4.Used = used
		currentPool.Status.IPv4.Available = available
		return true, nil
	})
}

// retryBackoff returns the wait before the retry following the given
// zero-based attempt: an exponential backoff (retryBaseDelay doubled per
// attempt, capped at retryMaxDelay) with full jitter, so the losers of one
// conflict round do not wake in lockstep and collide again. It never
// returns a negative duration and never exceeds retryMaxDelay.
func retryBackoff(retry int) time.Duration {
	backoff := retryBaseDelay << min(retry, retryShiftCap)
	if backoff > retryMaxDelay {
		backoff = retryMaxDelay
	}

	return time.Duration(rand.Int64N(int64(backoff) + 1))
}

// updatePoolStatus rebases only the intended status mutation on each fresh
// read. The callback reports whether a write is required; it must not perform
// allocation or release side effects, since conflicts invoke it again.
func updatePoolStatus(ctx context.Context, client *kihclientset.Clientset, networkName, poolName string, update func(*kihv1.IPPool) (bool, error)) error {
	for attempt := range maxAttempts {
		currentPool, err := client.KubevirtiphelperV1().IPPools().Get(ctx, poolName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("cannot get IPPool %s: %w", poolName, err)
		}
		if currentPool.Spec.NetworkName != networkName {
			return fmt.Errorf("cannot update status of IPPool %s: network %q does not match expected network %q", poolName, currentPool.Spec.NetworkName, networkName)
		}

		changed, err := update(currentPool)
		if err != nil || !changed {
			return err
		}
		currentPool.Status.LastUpdate = metav1.Now()
		if _, err := client.KubevirtiphelperV1().IPPools().UpdateStatus(ctx, currentPool, metav1.UpdateOptions{}); err == nil {
			return nil
		} else {
			if apierrors.IsConflict(err) || strings.Contains(err.Error(), "please apply your changes to the latest version and try again") {
				if attempt == maxAttempts-1 {
					return fmt.Errorf("cannot update status of IPPool %s after %d retries: %w", poolName, maxAttempts-1, err)
				}
			} else {
				return fmt.Errorf("cannot update status of IPPool %s: %w", poolName, err)
			}

			log.Warnf("(ippoolstatus.updatePoolStatus) cannot update status of IPPool %s after %d attempt(s), retrying in a bit",
				poolName, attempt+1)

			select {
			case <-ctx.Done():
				return fmt.Errorf("cannot update status of IPPool %s: %w", poolName, ctx.Err())
			case <-time.After(retryBackoff(attempt)):
			}
		}
	}

	return fmt.Errorf("cannot update status of IPPool %s after %d retries", poolName, maxAttempts-1)
}
