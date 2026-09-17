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
