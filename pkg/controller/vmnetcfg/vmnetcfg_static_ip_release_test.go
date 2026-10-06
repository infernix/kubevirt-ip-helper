package vmnetcfg

import (
	"net/http"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// Tests for the durable static-ip release marker on the allocation path: a
// withdrawn (or changed) request must have its lease, ipam claim and ledger
// record released instead of being adopted through the F02 quarantined-lease
// branch, and the marker must not fire for the case F02 exists for (a commit
// which failed before the spec recorded the assignment).

// withReleaseMarker stamps the release marker on a vmnetcfg object.
func withReleaseMarker(obj *kihv1.VirtualMachineNetworkConfig, entries map[string]string) *kihv1.VirtualMachineNetworkConfig {
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations[util.StaticIPReleaseAnnotationName] = util.EncodeStaticIPReleaseMarker(entries)

	return obj
}

// seedWithdrawnBinding reproduces the withdrawal state: the binding's own
// live lease, ipam claim and ledger record for ip, while the stored row of
// the binding records no address.
func seedWithdrawnBinding(e *testEnv, ip string) string {
	ownRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	if _, err := e.ipam.ReclaimIP(testNetwork, ip, ownRef); err != nil {
		e.t.Fatalf("claiming the withdrawn address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, ip, testNamespace+"/"+testVMName); err != nil {
		e.t.Fatalf("leasing the withdrawn address: %s", err)
	}
	e.seedPool(map[string]string{ip: ownRef})

	return ownRef
}

// TestVMNetCfgWithdrawnAddressIsReleasedNotAdopted pins the withdrawal: the
// marker releases the lease, the claim and the ledger record of the
// withdrawn address, and the interface returns to dynamic allocation.
func TestVMNetCfgWithdrawnAddressIsReleasedNotAdopted(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownRef := seedWithdrawnBinding(e, "10.0.0.1")

	// the withdrawn address is declared by another vm, so the fresh dynamic
	// allocation must land on the other pool address
	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		return []declaredAddress{declaredRecord("tenant-a", "declaring-vm", "net1", "02:00:00:00:00:99", "10.0.0.1")}, nil
	}

	vmnetcfg := withReleaseMarker(newVMNetCfg("", testMAC), map[string]string{
		util.StaticIPReleaseKey(testNetwork, testMAC): "10.0.0.1",
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("spec ip = %q, want the freshly allocated 10.0.0.2", got)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.2" {
		t.Errorf("lease ip = %s, want the freshly allocated 10.0.0.2", lease.ClientIP.String())
	}

	allocated := e.getStoredPool().Status.IPv4.Allocated
	if _, held := allocated["10.0.0.1"]; held {
		t.Errorf("ledger = %v, want the withdrawn address released", allocated)
	}
	if got := allocated["10.0.0.2"]; got != ownRef {
		t.Errorf("allocated[10.0.0.2] = %q, want the binding %q", got, ownRef)
	}

	// the consumed marker entry is gone
	marker, err := util.ParseStaticIPReleaseMarker(stored.Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	if len(marker) != 0 {
		t.Errorf("marker = %v, want the consumed entry cleared", marker)
	}

	// the withdrawn address is free for its declaring binding
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", "tenant-a/declaring-vm [02:00:00:00:00:99]"); err != nil {
		t.Errorf("the withdrawn address must be free for its declarer: %s", err)
	}
}

// TestVMNetCfgWithdrawalMarkerIsDurableAndIdempotent pins that the release is
// driven by the marker on the object alone (no in-memory withdrawal state),
// and that a marker whose release is already converged clears without a
// release.
func TestVMNetCfgWithdrawalMarkerIsDurableAndIdempotent(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	vmnetcfg := withReleaseMarker(newVMNetCfg("", testMAC), map[string]string{
		util.StaticIPReleaseKey(testNetwork, testMAC): "10.0.0.1",
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	// the pool holds two free addresses and AllocateIP ranges over a map, so
	// either address is a valid fresh dynamic allocation; the idempotent
	// no-op release is what this test pins, not the address choice
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" && got != "10.0.0.2" {
		t.Errorf("spec ip = %q, want a fresh dynamic address of the pool", got)
	}

	marker, err := util.ParseStaticIPReleaseMarker(stored.Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	if len(marker) != 0 {
		t.Errorf("marker = %v, want the idempotently absent entry cleared", marker)
	}
}

// TestVMNetCfgWithdrawalMarkerStaysUntilTheReleaseConverges pins that a
// failed release keeps the marker, so the retried sync repeats it, and a
// successful retry clears it.
func TestVMNetCfgWithdrawalMarkerStaysUntilTheReleaseConverges(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	seedWithdrawnBinding(e, "10.0.0.1")

	vmnetcfg := withReleaseMarker(newVMNetCfg("", testMAC), map[string]string{
		util.StaticIPReleaseKey(testNetwork, testMAC): "10.0.0.1",
	})
	e.seedVMNetCfg(vmnetcfg)

	// the ledger removal fails definitively: the release must not converge
	e.api.mu.Lock()
	e.api.poolStatusPutCode = http.StatusBadRequest
	e.api.mu.Unlock()

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("the sync must fail when the release of the withdrawn address fails")
	}

	stored := e.getStoredVMNetCfg()
	marker, err := util.ParseStaticIPReleaseMarker(stored.Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	if got := marker[util.StaticIPReleaseKey(testNetwork, testMAC)]; got != "10.0.0.1" {
		t.Errorf("marker = %v, want the unconverged release kept for the retry", marker)
	}
	// the binding is still held: nothing was released
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("lease ip = %s, want the still-held 10.0.0.1", lease.ClientIP.String())
	}

	// the retried sync releases and clears the marker
	e.api.mu.Lock()
	e.api.poolStatusPutCode = 0
	e.api.mu.Unlock()

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the retried sync must succeed: %s", err)
	}

	stored = e.getStoredVMNetCfg()
	marker, err = util.ParseStaticIPReleaseMarker(stored.Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	if len(marker) != 0 {
		t.Errorf("marker = %v, want the converged entry cleared", marker)
	}
	if allocated := e.getStoredPool().Status.IPv4.Allocated; len(allocated) != 1 {
		t.Errorf("ledger = %v, want exactly the fresh allocation", allocated)
	}
}

// TestVMNetCfgMarkerDiscriminatesWithdrawalFromFailedCommit pins the F02
// discriminator: the identical binding state (an empty row with the binding's
// own live lease) is adopted without the marker - the case F02 exists for -
// and released with it.
func TestVMNetCfgMarkerDiscriminatesWithdrawalFromFailedCommit(t *testing.T) {
	t.Run("without the marker the quarantined lease is adopted", func(t *testing.T) {
		e := newTestEnv(t)
		e.appStatus.Store(APP_RUNNING)
		e.addSubnet("10.0.0.1", "10.0.0.2")

		seedWithdrawnBinding(e, "10.0.0.1")

		// the address is declared by another vm: only the F02 adoption keeps
		// the served address continuously reserved
		e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
			return []declaredAddress{declaredRecord("tenant-a", "declaring-vm", "net1", "02:00:00:00:00:99", "10.0.0.1")}, nil
		}

		vmnetcfg := newVMNetCfg("", testMAC)
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" {
			t.Errorf("spec ip = %q, want the adopted 10.0.0.1", got)
		}
		if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.1" {
			t.Errorf("lease ip = %s, want the adopted 10.0.0.1", lease.ClientIP.String())
		}
	})

	t.Run("with the marker the withdrawn lease is released", func(t *testing.T) {
		e := newTestEnv(t)
		e.appStatus.Store(APP_RUNNING)
		e.addSubnet("10.0.0.1", "10.0.0.2")

		seedWithdrawnBinding(e, "10.0.0.1")

		e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
			return []declaredAddress{declaredRecord("tenant-a", "declaring-vm", "net1", "02:00:00:00:00:99", "10.0.0.1")}, nil
		}

		vmnetcfg := withReleaseMarker(newVMNetCfg("", testMAC), map[string]string{
			util.StaticIPReleaseKey(testNetwork, testMAC): "10.0.0.1",
		})
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
			t.Errorf("spec ip = %q, want the reallocated 10.0.0.2 (the withdrawn address was released)", got)
		}
		if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.2" {
			t.Errorf("lease ip = %s, want the reallocated 10.0.0.2", lease.ClientIP.String())
		}
	})
}

// TestVMNetCfgWithdrawalMarkerResolvesUnrecordedAddress pins the empty marker
// value: the vm controller could not resolve the withdrawn address because
// the earlier commit never recorded it, so the vmnetcfg controller resolves
// it from the binding's own live lease before releasing.
func TestVMNetCfgWithdrawalMarkerResolvesUnrecordedAddress(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	seedWithdrawnBinding(e, "10.0.0.1")

	e.controller.staticIPDeclarations = func(string) ([]declaredAddress, error) {
		return []declaredAddress{declaredRecord("tenant-a", "declaring-vm", "net1", "02:00:00:00:00:99", "10.0.0.1")}, nil
	}

	vmnetcfg := withReleaseMarker(newVMNetCfg("", testMAC), map[string]string{
		util.StaticIPReleaseKey(testNetwork, testMAC): "",
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("spec ip = %q, want the reallocated 10.0.0.2", got)
	}
	allocated := e.getStoredPool().Status.IPv4.Allocated
	if _, held := allocated["10.0.0.1"]; held {
		t.Errorf("ledger = %v, want the lease-resolved withdrawn address released", allocated)
	}
}

// TestVMNetCfgWithdrawalMarkerConsumedForRowRecordingIt pins the retry path:
// a row which already records the marker's address has nothing to release, so
// the marker is consumed without touching the restored binding.
func TestVMNetCfgWithdrawalMarkerConsumedForRowRecordingIt(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownRef := seedWithdrawnBinding(e, "10.0.0.1")

	vmnetcfg := withReleaseMarker(newVMNetCfg("10.0.0.1", testMAC), map[string]string{
		util.StaticIPReleaseKey(testNetwork, testMAC): "10.0.0.1",
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" {
		t.Errorf("spec ip = %q, want the recorded address kept", got)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("lease ip = %s, want the recorded address kept", lease.ClientIP.String())
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != ownRef {
		t.Errorf("allocated[10.0.0.1] = %q, want the binding %q", got, ownRef)
	}

	marker, err := util.ParseStaticIPReleaseMarker(stored.Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	if len(marker) != 0 {
		t.Errorf("marker = %v, want the converged entry cleared", marker)
	}
}

// TestStaticIPReleaseKeyCanonicalizes pins the marker key shape the vm and
// the vmnetcfg controllers must agree on.
func TestStaticIPReleaseKeyCanonicalizes(t *testing.T) {
	key := util.StaticIPReleaseKey(testNetwork, "02-00-00-00-00-01")
	want := testNetwork + "|02:00:00:00:00:01"
	if key != want {
		t.Errorf("StaticIPReleaseKey = %q, want %q", key, want)
	}
}
