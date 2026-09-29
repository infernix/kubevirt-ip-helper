package vmnetcfg

import (
	"strings"
	"testing"
)

// TestVMNetCfgExhaustedPoolPublishesTheRefusal pins the observable
// refusal of the exhausted-pool path which the E2E pool lane waits for
// (POOL-TWELFTH-REFUSED: a fresh binding against a full pool must
// carry the ERROR status). a fresh assignment which cannot get an
// address fails the sync so the pending allocations of this sync are
// unwound, but its ERROR status must be committed with that same sync:
// the rate-limited retry of handleErr forgets the key after five
// attempts (R03), so a status which only a retried sync would write
// never becomes observable at all - the E2E lane then expires on an
// empty status instead of the 260ms of the green run.
func TestVMNetCfgExhaustedPoolPublishesTheRefusal(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	if _, err := e.ipam.AllocateIP(testNetwork, "other-ns/other-vm"); err != nil {
		t.Fatalf("occupying the only address: %s", err)
	}
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("want the exhausted pool to fail the sync")
	}

	stored := e.getStoredVMNetCfg()
	if len(stored.Status.NetworkConfig) != 1 {
		t.Fatalf("stored status = %+v, want the refused nic's ERROR entry", stored.Status.NetworkConfig)
	}
	if got := stored.Status.NetworkConfig[0]; got.Status != "ERROR" || !strings.Contains(got.Message, "no more ips left") {
		t.Errorf("status = %+v, want ERROR with the exhausted-pool message", got)
	}

	// nothing was served: the refused binding keeps no lease and consumes
	// no address beyond the foreign allocation
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must be created for the refused nic")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want the single foreign allocation only", used)
	}

	// the retried sync converges on the recorded refusal instead of
	// failing again: with the ERROR status committed, the resync records
	// the fresh failure and settles the object (the rate-limited retry
	// budget is never driven into the retry exhaustion of R03)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, stored); err != nil {
		t.Fatalf("the retried sync must converge on the recorded refusal: %s", err)
	}

	after := e.getStoredVMNetCfg()
	if got := after.Status.NetworkConfig[0]; got.Status != "ERROR" || !strings.Contains(got.Message, "no more ips left") {
		t.Errorf("status after the retry = %+v, want the recorded ERROR", got)
	}
}
