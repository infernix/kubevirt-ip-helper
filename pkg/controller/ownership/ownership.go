package ownership

import (
	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

const VLANIDAnnotation = "kubevirtiphelper/vlan-id"

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

// Enabled reports whether this scope restricts ownership to one VLAN.
func (s *Scope) Enabled() bool {
	return s.vlanID != ""
}

// StampOwnership records this scope on obj. Disabled scopes leave metadata
// unchanged.
func (s *Scope) StampOwnership(obj metav1.Object) bool {
	if !s.Enabled() || obj == nil {
		return false
	}

	annotations := obj.GetAnnotations()
	if annotations != nil && annotations[VLANIDAnnotation] == s.vlanID {
		return false
	}
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[VLANIDAnnotation] = s.vlanID
	obj.SetAnnotations(annotations)

	return true
}

// OwnsIPPool reports whether pool belongs to this scope.
func (s *Scope) OwnsIPPool(pool *kihv1.IPPool) bool {
	if pool == nil {
		return false
	}
	if !s.Enabled() {
		return true
	}

	return pool.Annotations[VLANIDAnnotation] == s.vlanID
}

// OwnsVirtualMachineNetworkConfig reports whether vmnetcfg belongs to this
// scope. An explicit VLAN annotation is authoritative; unannotated objects
// fall back to their referenced networks for upgrade adoption.
func (s *Scope) OwnsVirtualMachineNetworkConfig(vmnetcfg *kihv1.VirtualMachineNetworkConfig) bool {
	if vmnetcfg == nil {
		return false
	}
	if !s.Enabled() {
		return true
	}
	if vlanID, annotated := vmnetcfg.Annotations[VLANIDAnnotation]; annotated {
		return vlanID == s.vlanID
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
	if !s.Enabled() {
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
