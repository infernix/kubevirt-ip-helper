package vmnetcfg

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
