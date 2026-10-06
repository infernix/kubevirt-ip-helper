package vmnetcfg

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// orphanSweepPassLimit bounds the number of orphaned bindings one sweep
// pass deletes. the pass is already bounded by the work it drives - one
// listing of the informer store, one authoritative vm read per binding
// whose vm the vm cache cannot confirm, and one delete per orphan - and
// this cap keeps a single pass from running away on a pathological store;
// whatever the cap leaves behind is swept by the next pass.
const orphanSweepPassLimit = 512

// orphanSweepInterval is how often the controller runs a sweep pass. a
// pass covers every managed binding in one round, so the interval bounds
// the time until a recovery starts, never the per-object drain rate: a
// full pool is freed by one pass, not by one binding per resync.
const orphanSweepInterval = resyncPeriod

// bindingLiveness classifies the existence of the VirtualMachine behind a
// controller-managed binding, modelled on the ippool controller's
// ownerLiveness. the verdict distinguishes the authoritative absence,
// which may sweep the binding, from an existence which could not be
// established: an unverified binding is interpreted fail-closed by every
// consumer which must neither release nor re-claim the reservations of a
// possibly-live vm.
type bindingLiveness int

const (
	// bindingLive: the vm exists (or the binding is not controller-managed,
	// or it carries no owned configuration), so the binding is not an
	// orphan and the regular reconciliation proceeds.
	bindingLive bindingLiveness = iota
	// bindingGone: the vm is definitively gone, so the binding is an
	// orphan: it is routed into the deletion flow and its reservations
	// must never be re-claimed.
	bindingGone
	// bindingUnverified: the vm's existence could not be established (a
	// transient live read failed, or no verifier is wired). neither sweep
	// the possibly-live binding nor allocate for a possibly-gone vm; the
	// next resync retries the classification.
	bindingUnverified
)

// vmExistsInCache reports the existence of one VirtualMachine as the local
// informer store knows it. known is false when the store cannot answer at
// all (no vm informer is wired, or its lookup failed), so the caller falls
// back to the authoritative live read. a store which says gone is never
// the tie-breaker on its own: the live read confirms it, so a vm which
// merely lags in the store is still found live and never swept.
func (c *Controller) vmExistsInCache(namespace string, name string) (exists bool, known bool) {
	if c.vmIndexer == nil {
		return false, false
	}

	_, exists, err := c.vmIndexer.GetByKey(namespace + "/" + name)
	if err != nil {
		return false, false
	}

	return exists, true
}

// declarationMemo memoizes the static-ip declarations of one sweep pass.
// the pass is the unit which pays the cluster-wide VirtualMachine LIST: it
// reads the declarations of every network it touches once, and every
// reconciliation which runs while the pass is active shares the successful
// read, so N orphans do not become N cluster-wide LISTs. a failed read is
// only remembered so the pass does not repeat it for every binding; the
// reconciliations retry the read themselves and fail closed, exactly like
// a failing seam does without a pass, so a transient failure cannot stick
// for the whole pass window.
type declarationMemo struct {
	mu        sync.Mutex
	byNetwork map[string][]declaredAddress
	attempted map[string]bool
}

func newDeclarationMemo() *declarationMemo {
	return &declarationMemo{
		byNetwork: make(map[string][]declaredAddress),
		attempted: make(map[string]bool),
	}
}

// lookup returns the recorded declarations of one network. recorded is
// false when the pass has no successful read of that network, so the caller
// performs its own read (with the in-sync retry) and records the outcome.
func (m *declarationMemo) lookup(networkName string) (declared []declaredAddress, recorded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	declared, recorded = m.byNetwork[networkName]

	return
}

// attemptedRead reports whether the pass already read the network, so the
// prefetch does not repeat a failing read once per binding.
func (m *declarationMemo) attemptedRead(networkName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.attempted[networkName]
}

// record stores the outcome of one declaration read: a success is shared
// with every reconciliation of the pass, a failure only marks the network
// as attempted.
func (m *declarationMemo) record(networkName string, declared []declaredAddress, declarationErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.attempted[networkName] = true

	if declarationErr == nil {
		m.byNetwork[networkName] = declared
	}
}

// beginDeclarationPass installs a fresh declaration memo for a sweep pass
// and returns it. it is guarded so a reconciliation running concurrently
// with the pass observes either the pass memo or none, never a torn value.
func (c *Controller) beginDeclarationPass() *declarationMemo {
	pass := newDeclarationMemo()

	c.declarationMu.Lock()
	c.declarationPass = pass
	c.declarationMu.Unlock()

	return pass
}

// endDeclarationPass drops the pass memo if it is still the active one, so
// the reconciliations which follow the pass read the declarations freshly
// again.
func (c *Controller) endDeclarationPass(pass *declarationMemo) {
	c.declarationMu.Lock()
	if c.declarationPass == pass {
		c.declarationPass = nil
	}
	c.declarationMu.Unlock()
}

// activeDeclarationPass returns the memo of the sweep pass currently
// running, or nil outside a pass.
func (c *Controller) activeDeclarationPass() *declarationMemo {
	c.declarationMu.Lock()
	defer c.declarationMu.Unlock()

	return c.declarationPass
}

// readStaticIPDeclarations reads the declarations of one network with the
// bounded in-sync retry shared by the allocation path and the sweep pass: a
// transient api error gets a short retry before the caller fails closed.
func (c *Controller) readStaticIPDeclarations(networkName string) ([]declaredAddress, error) {
	var declared []declaredAddress
	var declarationErr error

lookup:
	for attempt := range staticIPDeclarationLookupAttempts {
		declared, declarationErr = c.staticIPDeclarations(networkName)
		if declarationErr == nil {
			break
		}

		if attempt < staticIPDeclarationLookupAttempts-1 {
			select {
			case <-c.ctx.Done():
				declarationErr = c.ctx.Err()

				break lookup
			case <-time.After(staticIPDeclarationLookupBackoff):
			}
		}
	}

	return declared, declarationErr
}

// ownedNetworksOf returns the distinct networks of the binding's owned
// interfaces, in spec order first: the sweep pass warms the declaration
// memo for exactly the networks a reconciliation of this object could
// consult.
func (c *Controller) ownedNetworksOf(vmnetcfg *kihv1.VirtualMachineNetworkConfig) []string {
	seen := make(map[string]bool)
	networks := []string{}

	add := func(networkName string) {
		if networkName == "" || seen[networkName] {
			return
		}

		seen[networkName] = true
		networks = append(networks, networkName)
	}

	for _, nic := range c.scope.FilterSpec(vmnetcfg.Namespace, vmnetcfg.Spec.NetworkConfig) {
		add(nic.NetworkName)
	}

	for _, row := range c.scope.FilterStatus(vmnetcfg.Namespace, vmnetcfg.Status.NetworkConfig) {
		add(row.NetworkName)
	}

	return networks
}

// prefetchPassDeclarations reads the declarations of the binding's owned
// networks into the pass memo (once per network and pass). the read is
// best-effort: a failure is only remembered so the pass does not repeat it
// for every binding, while the reconciliations which share the memo retry
// the read themselves and fail closed - the sweep itself never depends on
// the declarations.
func (c *Controller) prefetchPassDeclarations(pass *declarationMemo, vmnetcfg *kihv1.VirtualMachineNetworkConfig) {
	if pass == nil || c.staticIPDeclarations == nil {
		return
	}

	for _, networkName := range c.ownedNetworksOf(vmnetcfg) {
		if pass.attemptedRead(networkName) {
			continue
		}

		declared, declarationErr := c.readStaticIPDeclarations(networkName)
		if declarationErr != nil {
			log.Warnf("(vmnetcfg.sweepOrphanedBindings) cannot read the static ip declarations of network %s for the sweep pass, the reconciliations sharing the pass memo read them again and fail closed: %s",
				networkName, declarationErr.Error())
		}

		pass.record(networkName, declared, declarationErr)
	}
}

// sweepOrphanedBindings runs one bounded sweep pass over every
// controller-managed binding the informer store holds: one listing covers
// every managed binding whose vm is gone, instead of one binding per
// reconcile and resync. the pass shares one static-ip declaration memo, so
// N orphans cost one cluster-wide VirtualMachine LIST per network. each
// classification consults the vm informer store first and the authoritative
// live read as the tie-breaker, so a vm which merely lags in the store is
// never swept. the pass returns the number of orphaned bindings it swept;
// the remainder above orphanSweepPassLimit is swept by the next pass.
func (c *Controller) sweepOrphanedBindings() (swept int) {
	if c.indexer == nil {
		return
	}

	pass := c.beginDeclarationPass()
	defer c.endDeclarationPass(pass)

	considered := 0
	limited := false

	for _, item := range c.indexer.List() {
		vmnetcfg, ok := item.(*kihv1.VirtualMachineNetworkConfig)
		if !ok || vmnetcfg.ObjectMeta.DeletionTimestamp != nil {
			continue
		}

		if swept >= orphanSweepPassLimit {
			limited = true

			break
		}

		considered++
		c.prefetchPassDeclarations(pass, vmnetcfg)

		if liveness, _ := c.sweepOrphanedBinding(vmnetcfg); liveness == bindingGone {
			swept++
		}
	}

	if swept != 0 || limited {
		log.Infof("(vmnetcfg.sweepOrphanedBindings) one sweep pass considered %d binding(s) and swept %d orphaned binding(s) (pass limit %d, limited %v)",
			considered, swept, orphanSweepPassLimit, limited)
	}

	return
}

// runOrphanSweep drives the sweep passes of this era: one immediately, so
// the orphans a restart or a dropped delete event inherited are recovered
// without waiting a full resync, and then one per orphanSweepInterval. the
// pass is the recovery unit, so the interval never becomes the drain rate.
func (c *Controller) runOrphanSweep(stopCh chan struct{}) {
	c.sweepOrphanedBindings()

	ticker := time.NewTicker(orphanSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			c.sweepOrphanedBindings()
		}
	}
}
