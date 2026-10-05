package vmnetcfg

import (
	"errors"
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

// TestVMNetCfgDeclarationLookupFailureFailsClosed pins the fail-closed rule
// for the fresh-allocation path: a declaration lookup failure must not
// allocate, so a dynamic allocation can never durably take a declared
// address. the failure is retried inside the sync, fails the sync with a
// wrapped error, and publishes neither a lease nor a ledger entry.
func TestVMNetCfgDeclarationLookupFailureFailsClosed(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	calls := 0
	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		calls++

		return nil, errors.New("api read failed")
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("a declaration lookup failure on the fresh-allocation path must fail the sync")
	}
	if !strings.Contains(err.Error(), "cannot read the static ip declarations") {
		t.Errorf("error = %q, want the wrapped declaration lookup failure", err)
	}

	// the bounded in-sync retry ran
	if calls != staticIPDeclarationLookupAttempts {
		t.Errorf("declaration lookups = %d, want %d", calls, staticIPDeclarationLookupAttempts)
	}

	stored := e.getStoredVMNetCfg()
	if stored.Spec.NetworkConfig[0].IPAddress != "" {
		t.Errorf("spec ip = %q, want none: an unprotected dynamic address must not be committed", stored.Spec.NetworkConfig[0].IPAddress)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must be published for an unprotected dynamic address")
	}
	if allocated := e.getStoredPool().Status.IPv4.Allocated; len(allocated) != 0 {
		t.Errorf("ledger = %v, want no entry for an unprotected dynamic address", allocated)
	}

	// the retried sync allocates once the declarations are readable
	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		return []declaredAddress{declaredRecord("tenant-a", "vm-a", "net1", "02:00:00:00:00:99", "10.0.0.1")}, nil
	}
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the retried sync must succeed: %s", err)
	}
	if got := e.getStoredVMNetCfg().Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("retried spec ip = %q, want the undeclared 10.0.0.2", got)
	}
}

// TestVMNetCfgRestoreReleasesAddressDeclaredByAnotherOwner pins the restore
// path consultation: a recorded address which another binding declares is
// released and reallocated, so the declaration is never durably shadowed by
// a dynamic binding.
func TestVMNetCfgRestoreReleasesAddressDeclaredByAnotherOwner(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", ownRef); err != nil {
		t.Fatalf("claiming the recorded address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("leasing the recorded address: %s", err)
	}
	e.seedPool(map[string]string{"10.0.0.1": ownRef})

	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		return []declaredAddress{declaredRecord("tenant-a", "declaring-vm", "net1", "02:00:00:00:00:99", "10.0.0.1")}, nil
	}

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("spec ip = %q, want the reallocated 10.0.0.2", got)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.2" {
		t.Errorf("lease ip = %s, want the reallocated 10.0.0.2", lease.ClientIP.String())
	}

	allocated := e.getStoredPool().Status.IPv4.Allocated
	if _, held := allocated["10.0.0.1"]; held {
		t.Errorf("ledger = %v, want the declared address released", allocated)
	}
	if got := allocated["10.0.0.2"]; got != ownRef {
		t.Errorf("allocated[10.0.0.2] = %q, want the reallocated binding %q", got, ownRef)
	}

	// the declared address is free for its declarer
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", "tenant-a/declaring-vm [02:00:00:00:00:99]"); err != nil {
		t.Errorf("the declared address must stay claimable by its declarer: %s", err)
	}
}

// TestVMNetCfgRestoreKeepsOwnDeclaredAddress pins the other side of the
// restore-path rule: a recorded address declared by the binding's own vm and
// nic is restored, never released.
func TestVMNetCfgRestoreKeepsOwnDeclaredAddress(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", ownRef); err != nil {
		t.Fatalf("claiming the recorded address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("leasing the recorded address: %s", err)
	}
	e.seedPool(map[string]string{"10.0.0.1": ownRef})

	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		return []declaredAddress{declaredRecord(testNamespace, testVMName, "net1", testMAC, "10.0.0.1")}, nil
	}

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" {
		t.Errorf("spec ip = %q, want the own declared address restored", got)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("lease ip = %s, want the own declared address kept", lease.ClientIP.String())
	}
}

// TestVMNetCfgDeclarationLookupScope pins the lookup scope: one call per
// reconciliation and network, on the fresh-allocation path and on the
// recorded-address restore path alike (the restore consults the declarations
// so an address another owner declares is released).
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

	t.Run("one lookup for a restored recorded address", func(t *testing.T) {
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

		if calls != 1 {
			t.Errorf("declaration lookups = %d, want 1: the restore consults the declarations", calls)
		}
		if got := e.getStoredVMNetCfg().Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
			t.Errorf("spec ip = %q, want the restored 10.0.0.2", got)
		}
	})
}
