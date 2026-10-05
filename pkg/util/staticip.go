package util

import (
	"encoding/json"
	"fmt"
	"net/netip"
)

// StaticIPAnnotationName carries the per-nic static ipv4 request of a
// VirtualMachine. The value is a json object keyed by the vm interface
// name, mirroring the mac-address annotation the vm controller already
// reads: {"net1":"10.0.0.50"} asks for that address on the interface
// named net1 of that vm. A network is identified by the interface name
// of the vm spec, never by a vlan number.
const StaticIPAnnotationName = "kubevirtiphelper.k8s.binbash.org/static-ip"

// StaticIPReleaseAnnotationName marks a VirtualMachineNetworkConfig whose
// VirtualMachine withdrew or changed a static-ip request. The value is a json
// object keyed by StaticIPReleaseKey(network, macaddress) and valued with the
// released address (possibly empty when the vm controller could not resolve
// it): it names the bindings whose lease, ipam claim and ledger record the
// vmnetcfg controller must release. The marker lives on the object, so the
// release survives a helper restart; the vmnetcfg controller clears the
// consumed entries once their release converged.
const StaticIPReleaseAnnotationName = "kubevirtiphelper.k8s.binbash.org/static-ip-release"

// HarvesterMACAnnotationName is the Harvester-assigned macaddress annotation
// of a VirtualMachine: a json object keyed by the vm interface name. The vm
// projection falls back to it while the vm spec does not carry a macaddress
// yet, and the static-ip declaration walk resolves the declaring interface's
// macaddress with the same precedence.
const HarvesterMACAnnotationName = "harvesterhci.io/mac-address"

// StaticIPReleaseKey names one binding of a release marker: the qualified
// network name and the canonical macaddress, joined with a "|" which neither
// a network name nor a macaddress can carry.
func StaticIPReleaseKey(networkName string, macAddress string) string {
	return networkName + "|" + CanonicalHWAddr(macAddress)
}

// ParseStaticIPReleaseMarker decodes the release marker of a vmnetcfg object
// into a map of StaticIPReleaseKey to released address. A missing, empty or
// empty-valued annotation yields no entries; a malformed value is reported as
// an error so the consumer can log it and treat the marker as absent.
func ParseStaticIPReleaseMarker(annotations map[string]string) (map[string]string, error) {
	if len(annotations) == 0 {
		return nil, nil
	}

	raw, exists := annotations[StaticIPReleaseAnnotationName]
	if !exists || raw == "" {
		return nil, nil
	}

	var marker map[string]string
	if err := json.Unmarshal([]byte(raw), &marker); err != nil {
		return nil, fmt.Errorf("static ip release marker %s does not parse as a json object of binding key to address: %w",
			StaticIPReleaseAnnotationName, err)
	}

	if len(marker) == 0 {
		return nil, nil
	}

	return marker, nil
}

// EncodeStaticIPReleaseMarker serializes a release marker. An empty marker
// encodes to the empty string, which a caller uses to drop the annotation.
func EncodeStaticIPReleaseMarker(marker map[string]string) string {
	if len(marker) == 0 {
		return ""
	}

	encoded, err := json.Marshal(marker)
	if err != nil {
		// the marker is a map[string]string: json.Marshal cannot fail on it
		return ""
	}

	return string(encoded)
}

// ParseStaticIPAnnotation decodes the static ip annotation of a vm into a
// map of interface name to a canonical ipv4 address. A missing, empty or
// empty-valued annotation yields no requests at all, so a vm without the
// annotation keeps its previous behavior. A malformed annotation is
// reported as an error: admission rejects the vm with it, while the
// controller logs it and ignores the annotation so a hand-edited vm can
// never wedge the projection of its other interfaces.
func ParseStaticIPAnnotation(annotations map[string]string) (map[string]string, error) {
	if len(annotations) == 0 {
		return nil, nil
	}

	raw, exists := annotations[StaticIPAnnotationName]
	if !exists || raw == "" {
		return nil, nil
	}

	var requested map[string]string
	if err := json.Unmarshal([]byte(raw), &requested); err != nil {
		return nil, fmt.Errorf("static ip annotation %s does not parse as a json object of interface name to address: %w",
			StaticIPAnnotationName, err)
	}

	normalized := make(map[string]string, len(requested))
	for nicName, ipAddress := range requested {
		if nicName == "" {
			return nil, fmt.Errorf("static ip annotation %s carries an empty interface name", StaticIPAnnotationName)
		}
		// an empty value means the interface requests no address: it is
		// not a malformed request, it is the absence of one
		if ipAddress == "" {
			continue
		}

		ip, err := netip.ParseAddr(ipAddress)
		if err != nil || !ip.Is4() {
			return nil, fmt.Errorf("static ip annotation value %q of interface %s is not an ipv4 address",
				ipAddress, nicName)
		}

		normalized[nicName] = ip.Unmap().String()
	}

	if len(normalized) == 0 {
		return nil, nil
	}

	return normalized, nil
}
