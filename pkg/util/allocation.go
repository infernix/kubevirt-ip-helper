package util

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"unicode"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
)

// ErrForeignOwner reports an IPPool status entry whose allocation
// reference belongs to another owner, so a controller must not remove it.
// callers classify the updateIPPoolStatus rejection with errors.Is.
var ErrForeignOwner = errors.New("allocation belongs to another owner")

// AllocationRef builds the canonical allocation reference of an IPAM
// reservation: the persisted IPPool status entries, the ipam owner tokens
// and the reclaim decisions all agree on this spelling, so a restoring
// binding producing it again stays idempotent. the mac address is stored
// in the canonical colon form.
func AllocationRef(namespace string, vmName string, hwAddr string) string {
	return fmt.Sprintf("%s/%s [%s]", namespace, vmName, CanonicalHWAddr(hwAddr))
}

// ParseAllocationRef splits an allocation reference built by AllocationRef
// back into its components. the mac address is returned in its canonical
// colon form whatever spelling the reference carries (net.ParseMAC accepts
// the dash and uppercase spellings of older revisions and hand-edited
// status), so a consumer never splits one logical owner in two by
// comparing the returned hardware address verbatim.
//
// the accepted grammar is exactly "namespace/vmname [macaddress]": one
// owner separator, nonempty components, no bracket or whitespace inside the
// owner, a closing bracket and a mac address net.ParseMAC accepts. a
// reference outside that grammar is reported as unparseable (ok=false) and
// stays a live claim for every consumer: the pool registration pins such an
// address conservatively instead of releasing it, and the deletion gate
// blocks on it rather than proving it orphaned.
func ParseAllocationRef(ref string) (namespace string, vmName string, hwAddr string, ok bool) {
	const ownerSeparator = " ["

	ownerSep := strings.LastIndex(ref, ownerSeparator)
	if ownerSep < 0 || !strings.HasSuffix(ref, "]") {
		return "", "", "", false
	}

	mac := ref[ownerSep+len(ownerSeparator) : len(ref)-1]
	owner := ref[:ownerSep]

	// exactly one separator, and an owner without brackets or whitespace:
	// an owner this helper never wrote must not be attributed to a
	// namespace/vmname pair the index then fails to find
	if strings.Count(owner, "/") != 1 || strings.ContainsAny(owner, "[]") || strings.ContainsFunc(owner, unicode.IsSpace) {
		return "", "", "", false
	}

	slashSep := strings.Index(owner, "/")

	namespace = owner[:slashSep]
	vmName = owner[slashSep+1:]

	if namespace == "" || vmName == "" || mac == "" || strings.ContainsAny(mac, "[]/") {
		return "", "", "", false
	}

	// only a valid mac address may act as the owner identity, so garbage
	// reference tails from hand-edited status stay unattributable
	parsed, err := net.ParseMAC(mac)
	if err != nil {
		return "", "", "", false
	}

	return namespace, vmName, CanonicalHWAddr(parsed.String()), true
}

// IsAlreadyReleased reports ipam outcomes which state that nothing about
// the given address is left to release: a subnet name without allocation
// state, an address without a live allocation or an address which is
// outside the registered subnet at all. a plain empty ip is deliberately
// excluded: that is a caller error and must surface.
func IsAlreadyReleased(err error) bool {
	return errors.Is(err, ipam.ErrSubnetNotFound) ||
		errors.Is(err, ipam.ErrIPAlreadyFree) ||
		errors.Is(err, ipam.ErrIPNotInCidr)
}

// IsUnusableIdentity reports cleanup outcomes for identities which can
// never hold state: an unparseable hardware address or ip cannot own a
// lease or an allocation, so the cleanup has already converged for such an
// entry and must not be retried forever.
func IsUnusableIdentity(err error) bool {
	return errors.Is(err, dhcp.ErrLeaseInvalidHwAddr) ||
		errors.Is(err, ipam.ErrIPInvalid)
}
