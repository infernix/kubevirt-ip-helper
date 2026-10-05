package vmnetcfg

import (
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// Tests for the static-ip declaration contract on the allocation path: a
// declared address is never handed out dynamically, and the nic whose vm
// declares one claims exactly that address.

// TestVMNetCfgDynamicAllocationSkipsDeclaredAddress pins the exclusion half
// of the contract: a dynamic allocation never takes an address some vm
// declared, so the declaring binding can still claim it.
func TestVMNetCfgDynamicAllocationSkipsDeclaredAddress(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	declaringRef := testNamespace + "/declaring-vm [02:00:00:00:00:99]"
	e.controller.staticIPDeclarations = func(networkName string) (map[string]string, error) {
		if networkName != testNetwork {
			t.Errorf("declaration lookup for %q, want %q", networkName, testNetwork)
		}

		return map[string]string{"02:00:00:00:00:99": "10.0.0.1"}, nil
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
	e.controller.staticIPDeclarations = func(string) (map[string]string, error) {
		calls++

		return map[string]string{testMAC: "10.0.0.2"}, nil
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

	e.controller.staticIPDeclarations = func(string) (map[string]string, error) {
		return map[string]string{testMAC: "10.0.0.2"}, nil
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

// TestVMNetCfgDeclarationLookupFailureIsFailSoft pins the fail-soft choice
// for a lookup failure: the reconciliation proceeds with the pre-existing
// allocation behavior (logged, not failed), so a transient api read never
// blocks the object's other interfaces.
func TestVMNetCfgDeclarationLookupFailureIsFailSoft(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	e.controller.staticIPDeclarations = func(string) (map[string]string, error) {
		return nil, errors.New("api read failed")
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("a declaration lookup failure must not fail the sync: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" && got != "10.0.0.2" {
		t.Errorf("spec ip = %q, want one of the pool addresses", got)
	}
	if got := stored.Status.NetworkConfig[0].Status; got != "OK" {
		t.Errorf("status = %q, want OK", got)
	}
	if v, ok := e.metricValue(metricAppLogs, map[string]string{"loglevel": "warning"}); !ok || v < 1 {
		t.Errorf("warning log metric = %v (present %v), want >= 1", v, ok)
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
		e.controller.staticIPDeclarations = func(string) (map[string]string, error) {
			calls++

			return map[string]string{testMAC: "10.0.0.1"}, nil
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
		e.controller.staticIPDeclarations = func(string) (map[string]string, error) {
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
