package vmnetcfg

import (
	log "github.com/sirupsen/logrus"
	kubevirtV1 "kubevirt.io/api/core/v1"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// declaredAddresses walks a cluster-wide VirtualMachine list and returns,
// for one network, the addresses which the static-ip annotations of those
// virtual machines declare, keyed by the canonical macaddress of the
// declaring interface.
//
// the walk is the read side of the declaration contract: a declared
// address must never be handed out by a dynamic allocation, and the nic
// whose vm declares it must claim exactly that address. the declaring
// interface is identified the same way the projection identifies it - by
// the vm interface name which the annotation keys on, resolved to its
// Multus networkName and qualified in the vm's namespace - so an entry for
// a nic on another network (or a non-Multus, an empty or a missing
// networkName) is left to the helper which serves that network.
//
// the function is pure and fail-soft: a malformed annotation is logged and
// skipped (the admission check rejects it, a hand-edited vm must not wedge
// the projection of its other interfaces), an interface without a
// macaddress cannot be keyed and is skipped until the vm controller
// assigned one, and a vm without the annotation contributes nothing.
func declaredAddresses(vms *kubevirtV1.VirtualMachineList, networkName string) map[string]string {
	if vms == nil || networkName == "" {
		return nil
	}

	declared := make(map[string]string)

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

		for _, nic := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
			requested, asked := requests[nic.Name]
			// an entry for an interface the vm does not have, or one whose
			// macaddress is not assigned yet, cannot be attributed to a
			// binding identity
			if !asked || nic.MacAddress == "" {
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

			declared[util.CanonicalHWAddr(nic.MacAddress)] = requested
		}
	}

	if len(declared) == 0 {
		return nil
	}

	return declared
}

// exclusionSet projects a declared-address map onto the address set which
// a fresh dynamic allocation must skip. an empty (or nil) declaration set
// yields a nil exclusion, which makes the excluding allocation identical
// to the plain one.
func exclusionSet(declared map[string]string) map[string]bool {
	if len(declared) == 0 {
		return nil
	}

	excluded := make(map[string]bool, len(declared))
	for _, declaredIP := range declared {
		excluded[declaredIP] = true
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
