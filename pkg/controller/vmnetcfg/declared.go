package vmnetcfg

import (
	"encoding/json"

	log "github.com/sirupsen/logrus"
	kubevirtV1 "kubevirt.io/api/core/v1"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// declaredAddress records one static-ip declaration of one virtual machine
// interface: the declaring vm (namespace and name), the interface name which
// the annotation keys on, the canonical macaddress of that interface when it
// can be resolved (the spec macaddress first, else the harvesterhci.io
// mac-address annotation, the same precedence the projection uses), and the
// declared address.
//
// the record carries the owner identity because a declaration is not a
// property of an address or of a macaddress alone: the declaring vm's binding
// claims exactly its own declaration, and an unannotated vm which happens to
// share a macaddress must never be attributed the declaration of another vm.
type declaredAddress struct {
	namespace string
	name      string
	nic       string
	mac       string
	address   string
}

// declaredAddresses walks a cluster-wide VirtualMachine list and returns the
// declarations of one network.
//
// the walk is the read side of the declaration contract: a declared address
// must never be handed out by a dynamic allocation, and the nic whose vm
// declares it must claim exactly that address. the declaring interface is
// identified the same way the projection identifies it - by the vm interface
// name which the annotation keys on, resolved to its Multus networkName and
// qualified in the vm's namespace - so an entry for a nic on another network
// (or a non-Multus, an empty or a missing networkName) is left to the helper
// which serves that network.
//
// the function is pure and fail-soft: a malformed annotation is logged and
// skipped (the admission check rejects it, a hand-edited vm must not wedge
// the projection of its other interfaces), and a vm without the annotation
// contributes nothing. an interface whose macaddress the projection cannot
// resolve either (no spec macaddress and no harvester mac-address entry)
// contributes its address to the exclusion union with an empty mac: it cannot
// be attributed to a binding identity yet, but a dynamic allocation must
// still never take its declared address.
func declaredAddresses(vms *kubevirtV1.VirtualMachineList, networkName string) []declaredAddress {
	if vms == nil || networkName == "" {
		return nil
	}

	var declared []declaredAddress

	for i := range vms.Items {
		vm := &vms.Items[i]
		if vm.Spec.Template == nil {
			continue
		}

		requests, err := util.ParseStaticIPAnnotation(vm.Annotations)
		if err != nil {
			log.Warnf("(vmnetcfg.declaredAddresses) [%s/%s] ignoring the static ip annotation: %s",
				vm.Namespace, vm.Name, err)

			continue
		}
		if len(requests) == 0 {
			continue
		}

		harvesterMacs := harvesterMacAddresses(vm)

		for _, nic := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
			requested, asked := requests[nic.Name]
			if !asked {
				continue
			}

			network := multusNetworkName(vm, nic.Name)
			if network == "" {
				continue
			}

			// the same qualification the projection and the admission
			// comparison use: an unqualified multus reference resolves in
			// the vm's namespace, a qualified one is independent of it
			if util.QualifyNetworkName(vm.Namespace, network) != util.QualifyNetworkName(vm.Namespace, networkName) {
				continue
			}

			// the effective macaddress follows the projection's precedence:
			// the spec macaddress wins, the harvester annotation is the
			// fallback for a vm whose spec does not carry one yet
			macAddress := nic.MacAddress
			if macAddress == "" {
				macAddress = harvesterMacs[nic.Name]
			}

			mac := ""
			if macAddress != "" {
				mac = util.CanonicalHWAddr(macAddress)
			}

			declared = append(declared, declaredAddress{
				namespace: vm.Namespace,
				name:      vm.Name,
				nic:       nic.Name,
				mac:       mac,
				address:   requested,
			})
		}
	}

	return declared
}

// harvesterMacAddresses decodes the harvesterhci.io mac-address annotation of
// a vm into a map of interface name to macaddress. a missing or malformed
// annotation yields no entries: the projection logs and ignores it too.
func harvesterMacAddresses(vm *kubevirtV1.VirtualMachine) map[string]string {
	if vm.Annotations == nil {
		return nil
	}

	raw, exists := vm.Annotations[util.HarvesterMACAnnotationName]
	if !exists || raw == "" {
		return nil
	}

	var macs map[string]string
	if err := json.Unmarshal([]byte(raw), &macs); err != nil {
		log.Warnf("(vmnetcfg.declaredAddresses) [%s/%s] ignoring the harvester mac-address annotation: %s",
			vm.Namespace, vm.Name, err)

		return nil
	}

	return macs
}

// declaredForBinding resolves the address which the given binding's own nic
// declares: the declaration must match the declaring vm's namespace and name
// and the nic's canonical macaddress. a declaration without a macaddress
// cannot be attributed to a binding identity, so it is only part of the
// exclusion union.
func declaredForBinding(declared []declaredAddress, namespace string, vmName string, macAddress string) (string, bool) {
	mac := util.CanonicalHWAddr(macAddress)

	for _, entry := range declared {
		if entry.namespace == namespace && entry.name == vmName && entry.mac != "" && entry.mac == mac {
			return entry.address, true
		}
	}

	return "", false
}

// declaredAddressOwnedBy reports whether the given address is declared by the
// binding's own vm and nic. a declaration without a macaddress is attributed
// to the vm: the vm declared the address, so serving it to one of that vm's
// nics honors the declaration.
func declaredAddressOwnedBy(declared []declaredAddress, namespace string, vmName string, macAddress string, address string) bool {
	mac := util.CanonicalHWAddr(macAddress)

	for _, entry := range declared {
		if entry.address != address {
			continue
		}
		if entry.namespace == namespace && entry.name == vmName && (entry.mac == "" || entry.mac == mac) {
			return true
		}
	}

	return false
}

// declaredAddressOfAnotherOwner reports whether the given address is declared
// by a binding other than the given one. a declaration without a macaddress
// of another vm still belongs to another owner.
func declaredAddressOfAnotherOwner(declared []declaredAddress, namespace string, vmName string, macAddress string, address string) bool {
	mac := util.CanonicalHWAddr(macAddress)

	for _, entry := range declared {
		if entry.address != address {
			continue
		}
		if entry.namespace == namespace && entry.name == vmName && (entry.mac == "" || entry.mac == mac) {
			continue
		}

		return true
	}

	return false
}

// exclusionSet projects the declarations onto the address set which a fresh
// dynamic allocation must skip. it is the union of every declared address,
// including the declarations which cannot be attributed to a binding identity
// (no macaddress) and the duplicate declarations of several nics: an address
// some vm declared is never handed out dynamically. an empty (or nil)
// declaration set yields a nil exclusion, which makes the excluding
// allocation identical to the plain one.
func exclusionSet(declared []declaredAddress) map[string]bool {
	if len(declared) == 0 {
		return nil
	}

	excluded := make(map[string]bool, len(declared))
	for _, entry := range declared {
		excluded[entry.address] = true
	}

	return excluded
}

// multusNetworkName resolves the networkName of the networks[] entry which
// the given interface name refers to. a missing entry, a non-Multus entry
// and an empty networkName all resolve to "", so the caller skips them.
func multusNetworkName(vm *kubevirtV1.VirtualMachine, interfaceName string) string {
	for _, network := range vm.Spec.Template.Spec.Networks {
		if network.Name != interfaceName {
			continue
		}
		if network.Multus == nil {
			return ""
		}

		return network.Multus.NetworkName
	}

	return ""
}
