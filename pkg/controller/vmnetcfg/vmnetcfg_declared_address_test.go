package vmnetcfg

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// Tests for the static-ip declaration contract on the allocation path: a
// declared address is never handed out dynamically, the nic whose vm declares
// one claims exactly that address, and a declaration belongs to the vm and
// the nic which made it.

// declaredRecord builds one declaration record of the walk.
func declaredRecord(namespace, name, nic, mac, address string) declaredAddress {
	return declaredAddress{namespace: namespace, name: name, nic: nic, mac: mac, address: address}
}

// TestVMNetCfgDynamicAllocationSkipsDeclaredAddress pins the exclusion half
// of the contract: a dynamic allocation never takes an address some vm
// declared, so the declaring binding can still claim it.
func TestVMNetCfgDynamicAllocationSkipsDeclaredAddress(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	declaringRef := "declaring-ns/declaring-vm [02:00:00:00:00:99]"
	e.controller.staticIPDeclarations = func(networkName string) ([]declaredAddress, error) {
		if networkName != testNetwork {
			t.Errorf("declaration lookup for %q, want %q", networkName, testNetwork)
		}

		return []declaredAddress{declaredRecord("declaring-ns", "declaring-vm", "net1", "02:00:00:00:00:99", "10.0.0.1")}, nil
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("spec ip = %q, want the undeclared 10.0.0.2", got)
	}
	if got := stored.Status.NetworkConfig[0].Status; got != "OK" {
		t.Errorf("status = %q, want OK", got)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.2" {
		t.Errorf("lease ip = %s, want 10.0.0.2", lease.ClientIP.String())
	}

	// the declared address stayed free for its declaring binding
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", declaringRef); err != nil {
		t.Errorf("the declared address must stay claimable by its declarer: %s", err)
	}
}

// TestVMNetCfgDeclaredAddressIsClaimed pins the belt-and-braces half: a row
// without an address whose vm declares one claims exactly that address
// instead of a dynamic allocation.
func TestVMNetCfgDeclaredAddressIsClaimed(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	calls := 0
	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		calls++

		return []declaredAddress{declaredRecord(testNamespace, testVMName, "net1", testMAC, "10.0.0.2")}, nil
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("spec ip = %q, want the declared 10.0.0.2", got)
	}
	if got := stored.Status.NetworkConfig[0]; got.Status != "OK" || got.Message != "IP address successfully allocated" {
		t.Errorf("status = %+v, want OK", got)
	}

	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.2" {
		t.Errorf("lease ip = %s, want the declared 10.0.0.2", lease.ClientIP.String())
	}

	ownerRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.2"]; got != ownerRef {
		t.Errorf("allocated = %q, want the declaring binding %q", got, ownerRef)
	}

	// exactly one declaration lookup per reconciliation and network
	if calls != 1 {
		t.Errorf("declaration lookups = %d, want 1", calls)
	}

	// the other pool address was never consumed by this sync
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", "other-ns/other-vm [02:00:00:00:00:99]"); err != nil {
		t.Errorf("the undeclared address must stay allocatable: %s", err)
	}
}

// TestVMNetCfgDeclaredAddressRefusedWhenTaken pins that a declaration is
// claimed owner-validated: an address another owner holds is refused with
// the usual ERROR status and failing sync, and the foreign claim survives.
func TestVMNetCfgDeclaredAddressRefusedWhenTaken(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	foreignRef := "other-ns/other-vm [02:00:00:00:00:99]"
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", foreignRef); err != nil {
		t.Fatalf("occupying the declared address: %s", err)
	}
	e.seedPool(map[string]string{"10.0.0.2": foreignRef})

	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		return []declaredAddress{declaredRecord(testNamespace, testVMName, "net1", testMAC, "10.0.0.2")}, nil
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("the sync must fail when the declared address belongs to another owner")
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Status.NetworkConfig[0]; got.Status != "ERROR" || !strings.Contains(got.Message, "already allocated by") {
		t.Errorf("status = %+v, want an ERROR naming the foreign owner", got)
	}
	if stored.Spec.NetworkConfig[0].IPAddress != "" {
		t.Errorf("spec ip = %q, want none: the refused declaration is not recorded", stored.Spec.NetworkConfig[0].IPAddress)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must be created for the refused declaration")
	}

	// the foreign claim is untouched
	if ip, found := e.ipam.IPOwnedBy(testNetwork, foreignRef); !found || ip != "10.0.0.2" {
		t.Errorf("foreign claim = %q (found %v), want 10.0.0.2", ip, found)
	}
}

// TestVMNetCfgDeclarationAttribution pins the ownership half of the contract:
// a declaration belongs to the vm which made it, so a binding of another vm
// which shares the declared macaddress never claims it, and the declared
// address stays free for its declarer.
func TestVMNetCfgDeclarationAttribution(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	declaringRef := testNamespace + "/declaring-vm [" + testMAC + "]"
	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		// the declaring vm shares this binding's macaddress
		return []declaredAddress{declaredRecord(testNamespace, "declaring-vm", "net1", testMAC, "10.0.0.1")}, nil
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("spec ip = %q, want the undeclared 10.0.0.2: the declaration belongs to declaring-vm", got)
	}

	// the declaration of the other vm stayed free for its own binding
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", declaringRef); err != nil {
		t.Errorf("the declared address must stay claimable by its declarer: %s", err)
	}
}

// TestVMNetCfgDynamicAllocationSkipsEverySameMACDeclaration pins that the
// exclusion is the union of every declared address: two declarations which
// share a macaddress must not overwrite each other, or a dynamic allocation
// takes the dropped one.
func TestVMNetCfgDynamicAllocationSkipsEverySameMACDeclaration(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.3")
	e.seedPool(nil)

	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		return []declaredAddress{
			declaredRecord("tenant-a", "vm-a", "net1", testMAC, "10.0.0.1"),
			declaredRecord("tenant-b", "vm-b", "net1", testMAC, "10.0.0.2"),
		}, nil
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.3" {
		t.Errorf("spec ip = %q, want the undeclared 10.0.0.3: both same-mac declarations are excluded", got)
	}

	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", "tenant-a/vm-a ["+testMAC+"]"); err != nil {
		t.Errorf("vm-a's declared address must stay claimable: %s", err)
	}
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", "tenant-b/vm-b ["+testMAC+"]"); err != nil {
		t.Errorf("vm-b's declared address must stay claimable: %s", err)
	}
}

// TestVMNetCfgDeclarationLookupScope pins the lookup scope: one call per
// reconciliation and network, none at all when no interface reaches the
// fresh-allocation path.
func TestVMNetCfgDeclarationLookupScope(t *testing.T) {
	t.Run("one lookup for two pending nics", func(t *testing.T) {
		e := newTestEnv(t)
		e.appStatus.Store(APP_RUNNING)
		e.addSubnet("10.0.0.1", "10.0.0.2")
		e.seedPool(nil)

		calls := 0
		e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
			calls++

			return []declaredAddress{declaredRecord(testNamespace, testVMName, "net1", testMAC, "10.0.0.1")}, nil
		}

		vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testVMNetCfgName},
			Spec: kihv1.VirtualMachineNetworkConfigSpec{
				VMName: testVMName,
				NetworkConfig: []kihv1.NetworkConfig{
					{MACAddress: testMAC, NetworkName: testNetwork},
					{MACAddress: testMAC2, NetworkName: testNetwork},
				},
			},
		}
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		if calls != 1 {
			t.Errorf("declaration lookups = %d, want 1 for both nics of the network", calls)
		}

		stored := e.getStoredVMNetCfg()
		addresses := map[string]string{}
		for _, row := range stored.Spec.NetworkConfig {
			addresses[row.MACAddress] = row.IPAddress
		}
		if addresses[testMAC] != "10.0.0.1" {
			t.Errorf("declaring nic ip = %q, want the declared 10.0.0.1", addresses[testMAC])
		}
		if addresses[testMAC2] != "10.0.0.2" {
			t.Errorf("dynamic nic ip = %q, want the remaining 10.0.0.2", addresses[testMAC2])
		}
	})

	t.Run("no lookup for a restored recorded address", func(t *testing.T) {
		e := newTestEnv(t)
		e.addSubnet("10.0.0.1", "10.0.0.2")
		e.seedPool(nil)

		calls := 0
		e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
			calls++

			return nil, nil
		}

		vmnetcfg := newVMNetCfg("10.0.0.2", testMAC)
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		if calls != 0 {
			t.Errorf("declaration lookups = %d, want none for a recorded address", calls)
		}
		if got := e.getStoredVMNetCfg().Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
			t.Errorf("spec ip = %q, want the restored 10.0.0.2", got)
		}
	})
}
