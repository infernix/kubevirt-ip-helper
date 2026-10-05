package vm

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// Tests for the durable static-ip release marker the vm controller records on
// the vmnetcfg object it owns: the marker names the (network, macaddress)
// tuple of a withdrawn or changed request and the address the binding held,
// so the vmnetcfg controller releases that binding instead of adopting the
// withdrawn address through its F02 quarantined-lease branch.

// TestStaticIPReleaseMarkerRecordsStoredAddress: a withdrawn request whose
// stored row records the address marks that address, and the same commit
// clears the row.
func TestStaticIPReleaseMarkerRecordsStoredAddress(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	vm := vmStaticIPAnnotatedVM("ns1", "vm1", "")
	vmStaticIPStoredVMNetCfg(f, "ns1", "vm1", mac, "10.0.0.42")
	c.staticIPReleases.record(releaseKey(vm), map[string]bool{"net1": true})

	if err := c.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("handleVirtualMachineObjectChange: %v", err)
	}

	updated := vmStaticIPLastUpdateBody(t, f)
	if len(updated.Spec.NetworkConfig) != 1 || updated.Spec.NetworkConfig[0].IPAddress != "" {
		t.Fatalf("stored row = %+v, want the withdrawn address cleared", updated.Spec.NetworkConfig)
	}

	marker, err := util.ParseStaticIPReleaseMarker(updated.Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	key := util.StaticIPReleaseKey("default/net-a", mac)
	if got := marker[key]; got != "10.0.0.42" {
		t.Errorf("marker[%s] = %q, want the withdrawn 10.0.0.42 (marker %v)", key, got, marker)
	}
}

// TestStaticIPReleaseMarkerResolvesUnrecordedAddressFromLease: an earlier
// sync whose commit never recorded the assignment leaves the row without an
// address while its lease serves one. The marker must still name that
// address, or the release would lose it and the F02 branch would re-adopt it.
func TestStaticIPReleaseMarkerResolvesUnrecordedAddressFromLease(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	vm := vmStaticIPAnnotatedVM("ns1", "vm1", "")
	vmStaticIPStoredVMNetCfg(f, "ns1", "vm1", mac, "")
	addSimpleLease(t, c.dhcp, mac, "10.0.0.42", "ns1/vm1")
	c.staticIPReleases.record(releaseKey(vm), map[string]bool{"net1": true})

	if err := c.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("handleVirtualMachineObjectChange: %v", err)
	}

	marker, err := util.ParseStaticIPReleaseMarker(vmStaticIPLastUpdateBody(t, f).Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	key := util.StaticIPReleaseKey("default/net-a", mac)
	if got := marker[key]; got != "10.0.0.42" {
		t.Errorf("marker[%s] = %q, want the leased 10.0.0.42 (marker %v)", key, got, marker)
	}
}

// TestStaticIPReleaseMarkerMergesWithPendingEntries: a release recorded while
// an earlier marker entry is still pending must not overwrite it, or the
// earlier release would never run.
func TestStaticIPReleaseMarkerMergesWithPendingEntries(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	vm := vmStaticIPAnnotatedVM("ns1", "vm1", "")
	vmStaticIPStoredVMNetCfg(f, "ns1", "vm1", mac, "10.0.0.42")

	pendingKey := util.StaticIPReleaseKey("default/net-a", "aa:bb:cc:00:00:99")
	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"].Annotations = map[string]string{
		util.StaticIPReleaseAnnotationName: util.EncodeStaticIPReleaseMarker(map[string]string{pendingKey: "10.0.0.99"}),
	}
	f.mu.Unlock()

	c.staticIPReleases.record(releaseKey(vm), map[string]bool{"net1": true})

	if err := c.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("handleVirtualMachineObjectChange: %v", err)
	}

	marker, err := util.ParseStaticIPReleaseMarker(vmStaticIPLastUpdateBody(t, f).Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	if got := marker[pendingKey]; got != "10.0.0.99" {
		t.Errorf("marker[%s] = %q, want the pending 10.0.0.99 (marker %v)", pendingKey, got, marker)
	}
	if got := marker[util.StaticIPReleaseKey("default/net-a", mac)]; got != "10.0.0.42" {
		t.Errorf("marker[%s] = %q, want the new 10.0.0.42 (marker %v)", util.StaticIPReleaseKey("default/net-a", mac), got, marker)
	}
}

// TestStaticIPReleaseMarkerNotWrittenWithoutReleases: an unchanged projection
// must not stamp the marker, or every vmnetcfg would carry it.
func TestStaticIPReleaseMarkerNotWrittenWithoutReleases(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	vm := vmStaticIPAnnotatedVM("ns1", "vm1", `{"net1":"10.0.0.50"}`)
	vmStaticIPStoredVMNetCfg(f, "ns1", "vm1", "aa:bb:cc:00:00:01", "10.0.0.42")

	if err := c.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("handleVirtualMachineObjectChange: %v", err)
	}

	updated := vmStaticIPLastUpdateBody(t, f)
	marker, err := util.ParseStaticIPReleaseMarker(updated.Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	if len(marker) != 0 {
		t.Errorf("marker = %v, want none without a release", marker)
	}
}

// TestStaticIPReleaseMarkerRecordedObjectShape pins the marker on a freshly
// created vmnetcfg object too: the create path funnels into the update
// projection, so a release recorded before the object existed still lands.
func TestStaticIPReleaseMarkerRecordedObjectShape(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	vm := vmStaticIPAnnotatedVM("ns1", "vm1", "")
	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        "vm1",
			NetworkConfig: []kihv1.NetworkConfig{testNetCfg(mac, "default/net-a", "10.0.0.42")},
		},
	}
	f.mu.Unlock()
	addSimpleLease(t, c.dhcp, mac, "10.0.0.42", "ns1/vm1")
	c.staticIPReleases.record(releaseKey(vm), map[string]bool{"net1": true})

	if err := c.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("handleVirtualMachineObjectChange: %v", err)
	}

	marker, err := util.ParseStaticIPReleaseMarker(vmStaticIPLastUpdateBody(t, f).Annotations)
	if err != nil {
		t.Fatalf("ParseStaticIPReleaseMarker: %v", err)
	}
	if got := marker[util.StaticIPReleaseKey("default/net-a", mac)]; got != "10.0.0.42" {
		t.Errorf("marker = %v, want the released 10.0.0.42", marker)
	}
}
