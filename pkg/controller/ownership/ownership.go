package ownership

import (
	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

const vlanIDAnnotation = "kubevirtiphelper/vlan-id"

// Scope decides whether resources belong to this controller instance.
type Scope struct {
	vlanID    string
	poolCache *cache.CacheAllocator
}

// New creates an ownership scope for vlanID and the pools owned by that scope.
func New(vlanID string, poolCache *cache.CacheAllocator) *Scope {
	return &Scope{
		vlanID:    vlanID,
		poolCache: poolCache,
	}
}

// OwnsIPPool reports whether pool belongs to this scope.
func (s *Scope) OwnsIPPool(pool *kihv1.IPPool) bool {
	if pool == nil {
		return false
	}
	if s.vlanID == "" {
		return true
	}

	return pool.Annotations[vlanIDAnnotation] == s.vlanID
}

// OwnsVirtualMachineNetworkConfig reports whether all networks referenced by
// vmnetcfg have pools in this scope.
func (s *Scope) OwnsVirtualMachineNetworkConfig(vmnetcfg *kihv1.VirtualMachineNetworkConfig) bool {
	if vmnetcfg == nil {
		return false
	}
	if s.vlanID == "" {
		return true
	}
	if len(vmnetcfg.Spec.NetworkConfig) == 0 {
		return false
	}

	for _, network := range vmnetcfg.Spec.NetworkConfig {
		if !s.ownsNetwork(network.NetworkName) {
			return false
		}
	}

	return true
}

// OwnsVirtualMachine reports whether all Multus networks referenced by vm have
// pools in this scope. Networks of other types are ignored.
func (s *Scope) OwnsVirtualMachine(vm *kubevirtv1.VirtualMachine) bool {
	if vm == nil {
		return false
	}
	if s.vlanID == "" {
		return true
	}
	if vm.Spec.Template == nil {
		return false
	}

	multusNetworks := 0
	for _, network := range vm.Spec.Template.Spec.Networks {
		if network.Multus == nil {
			continue
		}

		multusNetworks++
		if !s.ownsNetwork(network.Multus.NetworkName) {
			return false
		}
	}

	return multusNetworks > 0
}

func (s *Scope) ownsNetwork(networkName string) bool {
	if networkName == "" || s.poolCache == nil {
		return false
	}

	return s.poolCache.Check(&kihv1.IPPool{
		Spec: kihv1.IPPoolSpec{NetworkName: networkName},
	})
}
