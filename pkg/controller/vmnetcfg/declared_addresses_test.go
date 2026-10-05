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

func TestDeclaredAddressesWalk(t *testing.T) {
	t.Run("matching network", func(t *testing.T) {
		// the interface mac is spelled in the dashed form: the declaration
		// is keyed on the canonical form the binding identity uses
		vms := declaredTestList(staticIPTestVM("tenant-a", "vm-a", `{"net1":"10.0.0.5"}`, "net1", "02-00-00-00-00-01", testNetwork))

		declared := declaredAddresses(vms, testNetwork)
		if got := declared["02:00:00:00:00:01"]; got != "10.0.0.5" {
			t.Errorf("declared = %v, want the canonical-mac entry 10.0.0.5", declared)
		}
		if len(declared) != 1 {
			t.Errorf("declared entries = %d, want 1", len(declared))
		}
	})

	t.Run("unqualified reference resolves in the vm namespace", func(t *testing.T) {
		vms := declaredTestList(
			staticIPTestVM("default", "vm-default", `{"net1":"10.0.0.5"}`, "net1", "02:00:00:00:00:01", "net-test"),
			staticIPTestVM("tenant-a", "vm-tenant", `{"net1":"10.0.0.6"}`, "net1", "02:00:00:00:00:02", "net-test"),
		)

		declared := declaredAddresses(vms, testNetwork)
		if _, ok := declared["02:00:00:00:00:01"]; !ok {
			t.Errorf("declared = %v, want the default/net-test reference of vm-default", declared)
		}
		if _, ok := declared["02:00:00:00:00:02"]; ok {
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

	t.Run("missing interface and missing mac", func(t *testing.T) {
		vms := declaredTestList(
			staticIPTestVM("tenant-a", "vm-unknown-nic", `{"net-other":"10.0.0.5"}`, "net1", "02:00:00:00:00:01", testNetwork),
			staticIPTestVM("tenant-a", "vm-no-mac", `{"net1":"10.0.0.6"}`, "net1", "", testNetwork),
		)

		if declared := declaredAddresses(vms, testNetwork); len(declared) != 0 {
			t.Errorf("declared = %v, want none for an unknown interface or an unassigned mac", declared)
		}
	})

	t.Run("malformed annotation is skipped per vm", func(t *testing.T) {
		vms := declaredTestList(
			staticIPTestVM("tenant-a", "vm-malformed", `{"net1":"not-an-ip"}`, "net1", "02:00:00:00:00:01", testNetwork),
			staticIPTestVM("tenant-a", "vm-valid", `{"net1":"10.0.0.6"}`, "net1", "02:00:00:00:00:02", testNetwork),
		)

		declared := declaredAddresses(vms, testNetwork)
		if len(declared) != 1 {
			t.Fatalf("declared = %v, want exactly the valid vm's entry", declared)
		}
		if got := declared["02:00:00:00:00:02"]; got != "10.0.0.6" {
			t.Errorf("declared = %v, want the valid vm's 10.0.0.6", declared)
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

func TestExclusionSet(t *testing.T) {
	if excluded := exclusionSet(nil); excluded != nil {
		t.Errorf("exclusionSet(nil) = %v, want nil", excluded)
	}
	if excluded := exclusionSet(map[string]string{}); excluded != nil {
		t.Errorf("exclusionSet(empty) = %v, want nil", excluded)
	}

	excluded := exclusionSet(map[string]string{
		"02:00:00:00:00:01": "10.0.0.5",
		"02:00:00:00:00:02": "10.0.0.6",
	})
	if len(excluded) != 2 || !excluded["10.0.0.5"] || !excluded["10.0.0.6"] {
		t.Errorf("exclusionSet = %v, want both declared addresses", excluded)
	}
}
