package vmnetcfg

import (
	"testing"

	kubevirtV1 "kubevirt.io/api/core/v1"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// staticIPTestVM builds the minimal VirtualMachine shape the declaration
// walk reads: the annotation on the vm's own metadata, one interface and
// one networks[] entry. an empty multusNetwork builds a non-Multus entry
// when podNetwork is set, so the skip paths are expressible too.
func staticIPTestVM(namespace, name, annotation, interfaceName, macAddress, multusNetwork string) kubevirtV1.VirtualMachine {
	vm := kubevirtV1.VirtualMachine{}
	vm.Namespace = namespace
	vm.Name = name
	if annotation != "" {
		vm.Annotations = map[string]string{util.StaticIPAnnotationName: annotation}
	}

	networkSource := kubevirtV1.NetworkSource{}
	if multusNetwork == "" {
		networkSource.Pod = &kubevirtV1.PodNetwork{}
	} else {
		networkSource.Multus = &kubevirtV1.MultusNetwork{NetworkName: multusNetwork}
	}

	vm.Spec.Template = &kubevirtV1.VirtualMachineInstanceTemplateSpec{
		Spec: kubevirtV1.VirtualMachineInstanceSpec{
			Domain: kubevirtV1.DomainSpec{
				Devices: kubevirtV1.Devices{
					Interfaces: []kubevirtV1.Interface{{
						Name:       interfaceName,
						MacAddress: macAddress,
					}},
				},
			},
			Networks: []kubevirtV1.Network{{
				Name:          interfaceName,
				NetworkSource: networkSource,
			}},
		},
	}

	return vm
}

func declaredTestList(vms ...kubevirtV1.VirtualMachine) *kubevirtV1.VirtualMachineList {
	list := &kubevirtV1.VirtualMachineList{}
	list.Items = append(list.Items, vms...)

	return list
}

// declaredRecordFor returns the declaration of the given declaring vm.
func declaredRecordFor(declared []declaredAddress, namespace, name string) (declaredAddress, bool) {
	for _, entry := range declared {
		if entry.namespace == namespace && entry.name == name {
			return entry, true
		}
	}

	return declaredAddress{}, false
}

func TestDeclaredAddressesWalk(t *testing.T) {
	t.Run("matching network", func(t *testing.T) {
		// the interface mac is spelled in the dashed form: the record is
		// keyed on the canonical form the binding identity uses
		vms := declaredTestList(staticIPTestVM("tenant-a", "vm-a", `{"net1":"10.0.0.5"}`, "net1", "02-00-00-00-00-01", testNetwork))

		declared := declaredAddresses(vms, testNetwork)
		if len(declared) != 1 {
			t.Fatalf("declared = %v, want 1 record", declared)
		}
		want := declaredAddress{namespace: "tenant-a", name: "vm-a", nic: "net1", mac: "02:00:00:00:00:01", address: "10.0.0.5"}
		if declared[0] != want {
			t.Errorf("declared[0] = %+v, want %+v", declared[0], want)
		}
	})

	t.Run("unqualified reference resolves in the vm namespace", func(t *testing.T) {
		vms := declaredTestList(
			staticIPTestVM("default", "vm-default", `{"net1":"10.0.0.5"}`, "net1", "02:00:00:00:00:01", "net-test"),
			staticIPTestVM("tenant-a", "vm-tenant", `{"net1":"10.0.0.6"}`, "net1", "02:00:00:00:00:02", "net-test"),
		)

		declared := declaredAddresses(vms, testNetwork)
		if _, ok := declaredRecordFor(declared, "default", "vm-default"); !ok {
			t.Errorf("declared = %v, want the default/net-test reference of vm-default", declared)
		}
		if _, ok := declaredRecordFor(declared, "tenant-a", "vm-tenant"); ok {
			t.Errorf("declared = %v, tenant-a/net-test must not match default/net-test", declared)
		}
	})

	t.Run("foreign network", func(t *testing.T) {
		vms := declaredTestList(staticIPTestVM("tenant-a", "vm-a", `{"net1":"10.0.0.5"}`, "net1", "02:00:00:00:00:01", "other-ns/net-other"))

		if declared := declaredAddresses(vms, testNetwork); len(declared) != 0 {
			t.Errorf("declared = %v, want none for a foreign network", declared)
		}
	})

	t.Run("non-multus network", func(t *testing.T) {
		vms := declaredTestList(staticIPTestVM("tenant-a", "vm-a", `{"net1":"10.0.0.5"}`, "net1", "02:00:00:00:00:01", ""))

		if declared := declaredAddresses(vms, testNetwork); len(declared) != 0 {
			t.Errorf("declared = %v, want none for a pod network", declared)
		}
	})

	t.Run("missing interface", func(t *testing.T) {
		vms := declaredTestList(staticIPTestVM("tenant-a", "vm-unknown-nic", `{"net-other":"10.0.0.5"}`, "net1", "02:00:00:00:00:01", testNetwork))

		if declared := declaredAddresses(vms, testNetwork); len(declared) != 0 {
			t.Errorf("declared = %v, want none for an unknown interface", declared)
		}
	})

	t.Run("unresolved macaddress stays in the exclusion union", func(t *testing.T) {
		vms := declaredTestList(staticIPTestVM("tenant-a", "vm-no-mac", `{"net1":"10.0.0.6"}`, "net1", "", testNetwork))

		declared := declaredAddresses(vms, testNetwork)
		if len(declared) != 1 || declared[0].mac != "" || declared[0].address != "10.0.0.6" {
			t.Fatalf("declared = %+v, want one record with an empty mac and 10.0.0.6", declared)
		}
		if _, found := declaredForBinding(declared, "tenant-a", "vm-no-mac", "02:00:00:00:00:01"); found {
			t.Error("a declaration without a macaddress must not be attributed to a binding identity")
		}
		if excluded := exclusionSet(declared); !excluded["10.0.0.6"] {
			t.Errorf("exclusionSet = %v, want the mac-less declaration's address", excluded)
		}
	})

	t.Run("harvester macaddress resolves the declaring interface", func(t *testing.T) {
		vm := staticIPTestVM("tenant-a", "vm-harvester", `{"net1":"10.0.0.5"}`, "net1", "", testNetwork)
		vm.Annotations[util.HarvesterMACAnnotationName] = `{"net1":"02:00:00:00:00:07"}`

		declared := declaredAddresses(declaredTestList(vm), testNetwork)
		if len(declared) != 1 || declared[0].mac != "02:00:00:00:00:07" {
			t.Fatalf("declared = %+v, want the harvester-resolved macaddress", declared)
		}
		if got, found := declaredForBinding(declared, "tenant-a", "vm-harvester", "02:00:00:00:00:07"); !found || got != "10.0.0.5" {
			t.Errorf("declaredForBinding = %q (found %v), want the harvester-mac binding to claim 10.0.0.5", got, found)
		}
	})

	t.Run("spec macaddress wins over the harvester annotation", func(t *testing.T) {
		vm := staticIPTestVM("tenant-a", "vm-both", `{"net1":"10.0.0.5"}`, "net1", "02:00:00:00:00:01", testNetwork)
		vm.Annotations[util.HarvesterMACAnnotationName] = `{"net1":"02:00:00:00:00:07"}`

		declared := declaredAddresses(declaredTestList(vm), testNetwork)
		if len(declared) != 1 || declared[0].mac != "02:00:00:00:00:01" {
			t.Fatalf("declared = %+v, want the spec macaddress", declared)
		}
	})

	t.Run("malformed harvester annotation is ignored", func(t *testing.T) {
		vm := staticIPTestVM("tenant-a", "vm-bad-harvester", `{"net1":"10.0.0.5"}`, "net1", "", testNetwork)
		vm.Annotations[util.HarvesterMACAnnotationName] = `not json`

		declared := declaredAddresses(declaredTestList(vm), testNetwork)
		if len(declared) != 1 || declared[0].mac != "" || declared[0].address != "10.0.0.5" {
			t.Fatalf("declared = %+v, want the declaration with an unresolved macaddress", declared)
		}
	})

	t.Run("malformed annotation is skipped per vm", func(t *testing.T) {
		vms := declaredTestList(
			staticIPTestVM("tenant-a", "vm-malformed", `{"net1":"not-an-ip"}`, "net1", "02:00:00:00:00:01", testNetwork),
			staticIPTestVM("tenant-a", "vm-valid", `{"net1":"10.0.0.6"}`, "net1", "02:00:00:00:00:02", testNetwork),
		)

		declared := declaredAddresses(vms, testNetwork)
		if len(declared) != 1 {
			t.Fatalf("declared = %+v, want exactly the valid vm's record", declared)
		}
		if declared[0].name != "vm-valid" || declared[0].address != "10.0.0.6" {
			t.Errorf("declared = %+v, want the valid vm's 10.0.0.6", declared)
		}
	})

	t.Run("annotation on the template is not the vm annotation", func(t *testing.T) {
		vm := staticIPTestVM("tenant-a", "vm-a", "", "net1", "02:00:00:00:00:01", testNetwork)
		vm.Spec.Template.ObjectMeta.Annotations = map[string]string{util.StaticIPAnnotationName: `{"net1":"10.0.0.5"}`}

		if declared := declaredAddresses(declaredTestList(vm), testNetwork); len(declared) != 0 {
			t.Errorf("declared = %v, want none: the annotation belongs on the vm metadata", declared)
		}
	})

	t.Run("missing template, empty network and empty list", func(t *testing.T) {
		noTemplate := kubevirtV1.VirtualMachine{}
		noTemplate.Namespace = "tenant-a"
		noTemplate.Name = "vm-no-template"
		noTemplate.Annotations = map[string]string{util.StaticIPAnnotationName: `{"net1":"10.0.0.5"}`}

		if declared := declaredAddresses(declaredTestList(noTemplate), testNetwork); len(declared) != 0 {
			t.Errorf("declared = %v, want none without a template", declared)
		}
		if declared := declaredAddresses(declaredTestList(staticIPTestVM("tenant-a", "vm-a", `{"net1":"10.0.0.5"}`, "net1", "02:00:00:00:00:01", testNetwork)), ""); len(declared) != 0 {
			t.Errorf("declared = %v, want none for an empty network name", declared)
		}
		if declared := declaredAddresses(nil, testNetwork); len(declared) != 0 {
			t.Errorf("declared = %v, want none for a nil list", declared)
		}
	})
}

// TestDeclaredAddressOwnership pins the ownership half of the declaration
// contract: a declaration belongs to the vm which made it and to the nic
// which carries it, so an unannotated vm sharing a macaddress never claims
// another vm's declaration.
func TestDeclaredAddressOwnership(t *testing.T) {
	sharedMAC := "02:00:00:00:00:01"
	vms := declaredTestList(
		staticIPTestVM("tenant-a", "declaring-vm", `{"net1":"10.0.0.5"}`, "net1", sharedMAC, testNetwork),
		staticIPTestVM("tenant-a", "other-vm", "", "net1", sharedMAC, testNetwork),
	)

	declared := declaredAddresses(vms, testNetwork)
	if len(declared) != 1 {
		t.Fatalf("declared = %+v, want only the declaring vm's record", declared)
	}

	if got, found := declaredForBinding(declared, "tenant-a", "declaring-vm", sharedMAC); !found || got != "10.0.0.5" {
		t.Errorf("declaredForBinding(declaring-vm) = %q (found %v), want 10.0.0.5", got, found)
	}
	if got, found := declaredForBinding(declared, "tenant-a", "other-vm", sharedMAC); found {
		t.Errorf("declaredForBinding(other-vm) = %q, want none: the declaration is not its own", got)
	}
	if got, found := declaredForBinding(declared, "tenant-b", "declaring-vm", sharedMAC); found {
		t.Errorf("declaredForBinding(foreign namespace) = %q, want none", got)
	}

	if !declaredAddressOwnedBy(declared, "tenant-a", "declaring-vm", sharedMAC, "10.0.0.5") {
		t.Error("the declaring binding must own its declared address")
	}
	if declaredAddressOwnedBy(declared, "tenant-a", "other-vm", sharedMAC, "10.0.0.5") {
		t.Error("an unannotated vm sharing the macaddress must not own the declaration")
	}
	if !declaredAddressOfAnotherOwner(declared, "tenant-a", "other-vm", sharedMAC, "10.0.0.5") {
		t.Error("the declaration belongs to another owner for the unannotated vm")
	}
	if declaredAddressOfAnotherOwner(declared, "tenant-a", "declaring-vm", sharedMAC, "10.0.0.5") {
		t.Error("the declaring binding is not another owner of its own declaration")
	}
}

// TestDeclaredAddressUnionKeepsSameMACDeclarations pins that two declarations
// which share a macaddress both stay in the exclusion union: the index is not
// keyed on the macaddress alone.
func TestDeclaredAddressUnionKeepsSameMACDeclarations(t *testing.T) {
	sharedMAC := "02:00:00:00:00:01"
	vms := declaredTestList(
		staticIPTestVM("tenant-a", "vm-a", `{"net1":"10.0.0.5"}`, "net1", sharedMAC, testNetwork),
		staticIPTestVM("tenant-b", "vm-b", `{"net1":"10.0.0.6"}`, "net1", sharedMAC, testNetwork),
	)

	declared := declaredAddresses(vms, testNetwork)
	if len(declared) != 2 {
		t.Fatalf("declared = %+v, want both same-mac declarations", declared)
	}

	excluded := exclusionSet(declared)
	if len(excluded) != 2 || !excluded["10.0.0.5"] || !excluded["10.0.0.6"] {
		t.Errorf("exclusionSet = %v, want both declared addresses", excluded)
	}

	// each declaring binding claims exactly its own address
	if got, found := declaredForBinding(declared, "tenant-a", "vm-a", sharedMAC); !found || got != "10.0.0.5" {
		t.Errorf("vm-a declaration = %q (found %v), want 10.0.0.5", got, found)
	}
	if got, found := declaredForBinding(declared, "tenant-b", "vm-b", sharedMAC); !found || got != "10.0.0.6" {
		t.Errorf("vm-b declaration = %q (found %v), want 10.0.0.6", got, found)
	}
}

func TestExclusionSet(t *testing.T) {
	if excluded := exclusionSet(nil); excluded != nil {
		t.Errorf("exclusionSet(nil) = %v, want nil", excluded)
	}
	if excluded := exclusionSet([]declaredAddress{}); excluded != nil {
		t.Errorf("exclusionSet(empty) = %v, want nil", excluded)
	}

	excluded := exclusionSet([]declaredAddress{
		{namespace: "tenant-a", name: "vm-a", nic: "net1", mac: "02:00:00:00:00:01", address: "10.0.0.5"},
		{namespace: "tenant-a", name: "vm-b", nic: "net1", mac: "02:00:00:00:00:02", address: "10.0.0.6"},
	})
	if len(excluded) != 2 || !excluded["10.0.0.5"] || !excluded["10.0.0.6"] {
		t.Errorf("exclusionSet = %v, want both declared addresses", excluded)
	}
}
