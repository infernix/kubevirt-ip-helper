package service

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihipam "github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	log "github.com/sirupsen/logrus"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kubevirtV1 "kubevirt.io/api/core/v1"
)

// vmNetCfgAPIPath is the apiserver path of the kubevirtiphelper v1 group.
// The vmnetcfg and ippool objects are read through the generic path of the
// core RESTClient: the webhook only serves read-only list calls, so it
// reuses the core clientset it already holds instead of constructing a
// second, generated typed clientset.
const vmNetCfgAPIPath = "/apis/kubevirtiphelper.k8s.binbash.org/v1"

type Handler struct {
	ctx         context.Context
	kubeConfig  string
	kubeContext string
	clientset   *kubernetes.Clientset
	httpServer  *http.Server
}

// maxPoolAddrs mirrors the pool size cap of the helper's ipam
// (ipam.MaxPoolAddrs): a range larger than the cap is rejected by the
// controller's registration validation and would only produce a
// permanently unregistrable object.
const maxPoolAddrs = 65536

// ipv4Broadcast computes the broadcast address of an ipv4 prefix.
func ipv4Broadcast(prefix netip.Prefix) netip.Addr {
	addr := prefix.Addr().As4()

	bits := prefix.Bits()
	var broadcast [4]byte
	for i := range 4 {
		maskByte := byte(0)
		if remaining := bits - i*8; remaining > 0 {
			if remaining >= 8 {
				maskByte = 0xFF
			} else {
				maskByte = byte(0xFF) << (8 - remaining)
			}
		}

		broadcast[i] = addr[i] | ^maskByte
	}

	return netip.AddrFrom4(broadcast)
}

// ipv4RangeLen returns the number of addresses of the inclusive ipv4
// range between start and end, mirroring the helper's v4RangeLen.
func ipv4RangeLen(start netip.Addr, end netip.Addr) uint64 {
	startOctets := start.As4()
	endOctets := end.As4()

	startUint := binary.BigEndian.Uint32(startOctets[:])
	endUint := binary.BigEndian.Uint32(endOctets[:])

	return uint64(endUint-startUint) + 1
}

func Register(ctx context.Context, kubeConfig string, kubeContext string) *Handler {
	return &Handler{
		ctx:         ctx,
		kubeConfig:  kubeConfig,
		kubeContext: kubeContext,
	}
}

func (h *Handler) Init() {
	config, err := util.GetKubeConfig(h.kubeConfig, h.kubeContext)
	if err != nil {
		log.Panicf("%s", err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Panicf("%s", err.Error())
	}
	h.clientset = clientset
}

// listVirtualMachineNetworkConfigs lists the VirtualMachineNetworkConfig
// objects of one namespace. the call runs on the admission request's
// context (F08): when the apiserver's webhook timeout drops the request,
// the list aborts with it instead of lingering past the dropped caller.
func (h *Handler) listVirtualMachineNetworkConfigs(ctx context.Context, namespace string) (list *kihv1.VirtualMachineNetworkConfigList, err error) {
	raw, err := h.clientset.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("%s/namespaces/%s/virtualmachinenetworkconfigs", vmNetCfgAPIPath, namespace)).
		Do(ctx).Raw()
	if err != nil {
		return
	}

	list = &kihv1.VirtualMachineNetworkConfigList{}
	if err = json.Unmarshal(raw, list); err != nil {
		return
	}

	return
}

// listIPPools lists the IPPool objects. the ippools are cluster-scoped and
// are read through the generic apiserver path like the vmnetcfg objects.
// the call runs on the admission request's context (F08) like the other
// lists of the admission path.
func (h *Handler) listIPPools(ctx context.Context) (list *kihv1.IPPoolList, err error) {
	raw, err := h.clientset.CoreV1().RESTClient().Get().
		AbsPath("/apis/kubevirtiphelper.k8s.binbash.org/v1/ippools").
		Do(ctx).Raw()
	if err != nil {
		return
	}

	list = &kihv1.IPPoolList{}
	if err = json.Unmarshal(raw, list); err != nil {
		return
	}

	return
}

// listAllVirtualMachineNetworkConfigs lists the VirtualMachineNetworkConfig
// objects of every namespace. the call runs on the admission request's
// context (F08) like the other lists of the admission path.
func (h *Handler) listAllVirtualMachineNetworkConfigs(ctx context.Context) (list *kihv1.VirtualMachineNetworkConfigList, err error) {
	raw, err := h.clientset.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("%s/virtualmachinenetworkconfigs", vmNetCfgAPIPath)).
		Do(ctx).Raw()
	if err != nil {
		return
	}

	list = &kihv1.VirtualMachineNetworkConfigList{}
	if err = json.Unmarshal(raw, list); err != nil {
		return
	}

	return
}

// allocationOwnerKey identifies one network binding. An empty network in the
// index records an ambiguous live reference and blocks deletion conservatively
// for that owner and MAC on every network.
type allocationOwnerKey struct {
	namespace string
	vmName    string
	network   string
	hwAddr    string
}

type allocationOwnerIndex map[allocationOwnerKey]string

func buildAllocationOwnerIndex(list *kihv1.VirtualMachineNetworkConfigList) allocationOwnerIndex {
	index := allocationOwnerIndex{}

	for _, obj := range list.Items {
		if obj.Spec.VMName == "" {
			continue
		}

		for _, nc := range obj.Spec.NetworkConfig {
			if nc.MACAddress == "" {
				continue
			}

			hw, err := net.ParseMAC(nc.MACAddress)
			if err != nil {
				continue
			}

			key := allocationOwnerKey{
				namespace: obj.Namespace,
				vmName:    obj.Spec.VMName,
				network:   util.QualifyNetworkName(obj.Namespace, nc.NetworkName),
				hwAddr:    hw.String(),
			}
			if _, exists := index[key]; !exists {
				index[key] = obj.Name
			}
		}
	}

	return index
}

// evaluateIPPoolRecords splits the allocation records of an IPPool into the
// ones which block its deletion and the orphaned ones which do not. a
// record blocks when its owner/network/MAC tuple is backed by a live
// VirtualMachineNetworkConfig, when the index is unavailable, or when its
// owner or network is ambiguous. Only a provably orphaned record stops
// blocking. The returned slices are ordered by IP for deterministic denials.
func evaluateIPPoolRecords(allocated map[string]string, network string, index allocationOwnerIndex, indexAvailable bool) (blocking []string, orphaned []string) {
	ips := make([]string, 0, len(allocated))
	for ip := range allocated {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	network = util.QualifyNetworkName("", network)

	for _, ip := range ips {
		ref := allocated[ip]
		if ref == "EXCLUDED" {
			continue
		}

		if !indexAvailable {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to %q", ip, ref))

			continue
		}

		namespace, vmName, hwAddr, ok := util.ParseAllocationRef(ref)
		if !ok {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to the unparseable reference %q", ip, ref))

			continue
		}

		if network == "" {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to %q (ambiguous pool network)", ip, ref))

			continue
		}

		key := allocationOwnerKey{namespace: namespace, vmName: vmName, network: network, hwAddr: hwAddr}

		if objName, live := index[key]; live {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to %s (VirtualMachineNetworkConfig %s/%s)", ip, ref, namespace, objName))

			continue
		}

		key.network = ""
		if objName, ambiguous := index[key]; ambiguous {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to %s (VirtualMachineNetworkConfig %s/%s has an ambiguous network)", ip, ref, namespace, objName))

			continue
		}

		orphaned = append(orphaned, fmt.Sprintf("ip %s (%s)", ip, ref))
	}

	return blocking, orphaned
}

// validateIPPool rejects the deletion of an IPPool whose allocation records
// are still backed by a live VirtualMachineNetworkConfig. an allocation
// record whose owner tuple no longer has any live object - for example the
// record a deleted hand-created vmnetcfg without the cleanup finalizer
// leaves behind, which the helper only revalidates at its next service era -
// is orphaned and does not block the deletion: without this the pool stays
// undeletable until a leader restart.
//
// the lookup errs toward blocking: a failed cluster-wide list keeps every
// record blocking (the gate is then exactly the old one), and an unparseable
// reference can never be proven orphaned either. only a record whose
// (namespace, vmname, canonical network, canonical macaddress) matches no
// live object stops blocking.
func (h *Handler) validateIPPool(ctx context.Context, ar *admissionv1.AdmissionReview, pool *kihv1.IPPool) *admissionv1.AdmissionResponse {
	allow := &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}

	index := allocationOwnerIndex{}
	indexAvailable := true

	if len(pool.Status.IPv4.Allocated) > 0 {
		list, err := h.listAllVirtualMachineNetworkConfigs(ctx)
		if err != nil {
			indexAvailable = false
			log.Errorf("(service.validateIPPool) cannot list the VirtualMachineNetworkConfigs, every allocation record of IPPool %s blocks the deletion: %s",
				pool.Name, err.Error())
		} else {
			index = buildAllocationOwnerIndex(list)
		}
	}

	blocking, orphaned := evaluateIPPoolRecords(pool.Status.IPv4.Allocated, pool.Spec.NetworkName, index, indexAvailable)

	if len(blocking) > 0 {
		log.Warnf("(service.validateIPPool) denying the deletion of IPPool %s: %s", pool.Name, strings.Join(blocking, "; "))

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("ippool is still in use: %s", strings.Join(blocking, "; ")),
			},
		}
	}

	if len(orphaned) > 0 {
		log.Warnf("(service.validateIPPool) IPPool %s carries allocation records without a live VirtualMachineNetworkConfig (%s); they do not block the deletion",
			pool.Name, strings.Join(orphaned, ", "))
	}

	return allow
}

// findRecordedTuple reports whether another object of the list records the
// (vmname, macaddress) pair of one of the network interfaces of the
// admitted object, and returns the denial message naming the conflicting
// object and both networks. the check is network-agnostic: the dhcp
// allocator of the helper keys its lease map on the macaddress alone, so
// the same pair on different networks oscillates the one lease just the
// same. the comparison runs on the canonical form of the macaddress
// (R10): the allocator parses the address before keying its lease, so a
// dash or uppercase spelling of the same address is the same lease, and
// a raw-string comparison let a canonically equivalent duplicate through
// the guard. the same-vmname scope and the object-identity exemption (an
// object never conflicts with itself) are part of the check: a different
// vmname claiming the macaddress of another vm stays admissible.
func findRecordedTuple(obj *kihv1.VirtualMachineNetworkConfig, list *kihv1.VirtualMachineNetworkConfigList) (denied *string) {
	for _, nc := range obj.Spec.NetworkConfig {
		if nc.MACAddress == "" {
			continue
		}

		macAddress := util.CanonicalHWAddr(nc.MACAddress)

		for _, other := range list.Items {
			if other.Name == obj.Name || other.Spec.VMName != obj.Spec.VMName {
				continue
			}

			for _, onc := range other.Spec.NetworkConfig {
				if util.CanonicalHWAddr(onc.MACAddress) == macAddress {
					msg := fmt.Sprintf(
						"vmname %s is already recorded with macaddress %s by VirtualMachineNetworkConfig %s/%s (network %s): distinct objects cannot claim the same vm and macaddress, regardless of network",
						obj.Spec.VMName, nc.MACAddress, other.Namespace, other.Name, onc.NetworkName,
					)

					return &msg
				}
			}
		}
	}

	return nil
}

// validateVmNetCfgMACAddresses rejects a network interface whose
// macaddress cannot serve as a source address: a macaddress with the
// individual/group bit set (every multicast address, and the broadcast
// address) can never be held by a guest interface.
//
// this check is deliberately stricter than the helper controller, unlike
// the mirroring checks: the controller registers such a binding without a
// complaint (observed live: a multicast macaddress allocated an address
// with status OK), and the reservation then silently consumes the pool
// capacity because no guest can ever claim it - deleting the object leaks
// the record like any hand-created vmnetcfg. the check cannot reject a
// controller-created binding: the macaddress of a vm interface is assigned
// through kubemacpool, which does not hand out multicast addresses.
func (h *Handler) validateVmNetCfgMACAddresses(obj *kihv1.VirtualMachineNetworkConfig) (denied *string) {
	for _, nc := range obj.Spec.NetworkConfig {
		if msg := checkNICMACAddress(nc); msg != nil {
			return msg
		}
	}

	return nil
}

// checkNICMACAddress rejects the macaddress of one network interface when
// it cannot serve as a source address: a macaddress with the
// individual/group bit set (every multicast address, and the broadcast
// address) can never be held by a guest interface.
func checkNICMACAddress(nc kihv1.NetworkConfig) (denied *string) {
	if nc.MACAddress == "" {
		return nil
	}

	hw, err := net.ParseMAC(nc.MACAddress)
	if err != nil {
		msg := fmt.Sprintf("the macaddress %s of network %s does not parse", nc.MACAddress, nc.NetworkName)

		return &msg
	}

	if hw[0]&0x01 != 0 {
		msg := fmt.Sprintf("the macaddress %s of network %s has the multicast bit set: no interface can hold it as its source address, so the reservation could never serve a guest and would only consume the allocation", nc.MACAddress, nc.NetworkName)

		return &msg
	}

	return nil
}

// ipPoolByNetwork indexes the IPPools by their canonical network name, keeping
// only the pools which carry the identity of that network: util.PoolNetwork
// requires the pool's spec.networkname to be the qualified namespace/name
// reference and its network and network-namespace labels to name the same
// namespace/name. that is exactly the identity the helper's own registration
// selector and check require, so an unlabelled or foreign-labelled pool which
// merely carries the same spec.networkname can no longer win the index and
// supply the wrong range, excludes or ledger to the vmnetcfg and static-vm
// admission checks.
//
// several pools can still carry the same identity: the helper's registration
// rejects the duplicate at its next sync (its cache is keyed by the network
// name), but admission cannot see that race. the newest by creation timestamp
// wins, with the pool name as the deterministic tie-break (an absent timestamp
// counts as the oldest), so the selected pool does not depend on list order.
func ipPoolByNetwork(pools *kihv1.IPPoolList) map[string]*kihv1.IPPool {
	poolByNetwork := map[string]*kihv1.IPPool{}

	for i := range pools.Items {
		pool := &pools.Items[i]

		network := util.PoolNetwork(pool)
		if network == "" {
			continue
		}

		current, exists := poolByNetwork[network]
		if !exists || newerPool(pool, current) {
			poolByNetwork[network] = pool
		}
	}

	return poolByNetwork
}

// newerPool reports whether candidate is the pool to prefer of two pools which
// carry the same network identity: the newest creation timestamp wins, and the
// greater name breaks a timestamp tie.
func newerPool(candidate, current *kihv1.IPPool) bool {
	candidateCreated := candidate.CreationTimestamp.Time
	currentCreated := current.CreationTimestamp.Time

	if !candidateCreated.Equal(currentCreated) {
		return candidateCreated.After(currentCreated)
	}

	return candidate.Name > current.Name
}

// validateVmNetCfgIPAddresses rejects the explicit ipaddress of a
// VirtualMachineNetworkConfig which does not lie between the start and the
// end of the allocation range of the IPPool serving its networkname. the
// controller of the helper refuses such an interface too, but only after the
// object is stored: the nic is recorded with a permanent ERROR status and
// its rejection is re-logged on every rate-limited retry for the lifetime
// of the object (observed live with an out-of-range address re-attempted
// every few seconds). denying it at admission keeps the invalid object out
// of the cluster entirely.
//
// the check only runs when an IPPool for the networkname exists: a vmnetcfg
// whose network has no pool yet is the intended ordering of a vm created
// before its pool, and the controller's ERROR-then-recover path (the failed
// nic is re-attempted and converges to OK once the pool appears) is its
// observed contract - the admission check must not break it. internal
// failures fail open like the other vmnetcfg checks: the controller's own
// range validation stays the authoritative guard.
func (h *Handler) validateVmNetCfgIPAddresses(ctx context.Context, obj *kihv1.VirtualMachineNetworkConfig) (denied *string) {
	lookupNeeded := false
	for _, nc := range obj.Spec.NetworkConfig {
		if nc.IPAddress != "" && nc.NetworkName != "" {
			lookupNeeded = true

			break
		}
	}

	if !lookupNeeded {
		return nil
	}

	pools, err := h.listIPPools(ctx)
	if err != nil {
		log.Errorf("(service.validateVmNetCfgIPAddresses) cannot list the IPPools, allowing the request: %s", err.Error())

		return nil
	}

	poolByNetwork := ipPoolByNetwork(pools)

	for _, nc := range obj.Spec.NetworkConfig {
		if msg := checkNICIPAddress(nc, poolByNetwork[util.QualifyNetworkName(obj.Namespace, nc.NetworkName)]); msg != nil {
			return msg
		}
	}

	return nil
}

// checkNICIPAddress rejects the explicit ipaddress of one network
// interface when it does not lie between the start and the end of the
// allocation range of the IPPool serving its networkname. a nil pool (no
// IPPool serves the network yet) and a pool whose range does not parse are
// both allowed: the first is the intended ordering of a vm created before
// its pool, the second is the ippool controller's own projection rejection
// to handle.
func checkNICIPAddress(nc kihv1.NetworkConfig, pool *kihv1.IPPool) (denied *string) {
	if nc.IPAddress == "" || nc.NetworkName == "" || pool == nil {
		return nil
	}

	ip, err := netip.ParseAddr(nc.IPAddress)
	if err != nil || !ip.Is4() {
		msg := fmt.Sprintf("ipaddress %s of network %s does not parse as an ipv4 address (IPPool %s)",
			nc.IPAddress, nc.NetworkName, pool.Name)

		return &msg
	}

	start, startErr := netip.ParseAddr(pool.Spec.IPv4Config.Pool.Start)
	end, endErr := netip.ParseAddr(pool.Spec.IPv4Config.Pool.End)
	if startErr != nil || endErr != nil || !start.Is4() || !end.Is4() {
		return nil
	}

	if ip.Compare(start) < 0 || ip.Compare(end) > 0 {
		msg := fmt.Sprintf("ipaddress %s is not between the pool range %s..%s of network %s (IPPool %s): the controller would record the network interface with a permanent ERROR status",
			nc.IPAddress, pool.Spec.IPv4Config.Pool.Start, pool.Spec.IPv4Config.Pool.End, nc.NetworkName, pool.Name)

		return &msg
	}

	return nil
}

// changedNetworkConfigs subtracts complete stored rows as a multiset, rather
// than matching by position or canonical identity. Reorders and removals need
// no validation; each extra duplicate or modified row does. Changing the VM
// owner invalidates every remaining row's previous admission.
func changedNetworkConfigs(obj, old *kihv1.VirtualMachineNetworkConfig) []kihv1.NetworkConfig {
	if obj.Spec.VMName != old.Spec.VMName {
		return obj.Spec.NetworkConfig
	}

	remaining := make(map[kihv1.NetworkConfig]int, len(old.Spec.NetworkConfig))
	for _, nc := range old.Spec.NetworkConfig {
		remaining[nc]++
	}
	var changed []kihv1.NetworkConfig
	for _, nc := range obj.Spec.NetworkConfig {
		if remaining[nc] > 0 {
			remaining[nc]--
		} else {
			changed = append(changed, nc)
		}
	}
	return changed
}

// validateVmNetCfg rejects a VirtualMachineNetworkConfig which records a
// (vmname, macaddress) pair that another object of the same namespace
// already records. the controllers of the kubevirt-ip-helper key the lease
// ownership on the spec's vmname and the dhcp allocator keys its lease map
// on the macaddress alone, so two objects carrying the same vm and
// macaddress are indistinguishable to them - on any network: their
// contradictory specs are both honored as an address or network change of
// the same owner and the one lease oscillates between them on every resync
// while both report status OK.
//
// the check deliberately only covers the same-vmname case. a different
// vmname claiming the macaddress of another vm stays admissible: the
// controller refuses it with an ERROR status, which is the observed contract
// of the helper. internal failures fail open for the same reason: the
// controller guards remain the authoritative defense and a webhook fault
// must not block the controller's own vmnetcfg writes.
func (h *Handler) validateVmNetCfg(ctx context.Context, ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	allow := &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}

	obj := &kihv1.VirtualMachineNetworkConfig{}
	if err := json.Unmarshal(ar.Request.Object.Raw, obj); err != nil {
		log.Errorf("cannot unmarshal json to vmnetcfg: %s", err)

		return allow
	}

	if obj.Name == "" || obj.Spec.VMName == "" {
		return allow
	}

	if ar.Request.Operation == admissionv1.Update {
		old := &kihv1.VirtualMachineNetworkConfig{}
		if err := json.Unmarshal(ar.Request.OldObject.Raw, old); err != nil {
			// Without a usable old object no row can be proven unchanged.
			log.Errorf("cannot unmarshal old vmnetcfg, validating every row: %s", err)
		} else {
			obj.Spec.NetworkConfig = changedNetworkConfigs(obj, old)
		}
	}
	if len(obj.Spec.NetworkConfig) == 0 {
		return allow
	}

	if msg := h.validateVmNetCfgMACAddresses(obj); msg != nil {
		log.Warnf("(service.validateVmNetCfg) denying VirtualMachineNetworkConfig %s/%s: %s",
			obj.Namespace, obj.Name, *msg)

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: *msg,
			},
		}
	}

	if msg := h.validateVmNetCfgIPAddresses(ctx, obj); msg != nil {
		log.Warnf("(service.validateVmNetCfg) denying VirtualMachineNetworkConfig %s/%s: %s",
			obj.Namespace, obj.Name, *msg)

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: *msg,
			},
		}
	}
	list, err := h.listVirtualMachineNetworkConfigs(ctx, obj.Namespace)
	if err != nil {
		log.Errorf("cannot list the VirtualMachineNetworkConfigs of namespace %s, allowing the request: %s",
			obj.Namespace, err.Error())

		return allow
	}

	if msg := findRecordedTuple(obj, list); msg != nil {
		log.Warnf("(service.validateVmNetCfg) denying VirtualMachineNetworkConfig %s/%s: %s",
			obj.Namespace, obj.Name, *msg)

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: *msg,
			},
		}
	}

	return allow
}

// writeAdmissionResponse writes exactly one well-formed AdmissionReview
// response: the single-write invariant of every admission handler lives
// here, so a response path can never half-write a status code and then
// fall through into a second write of the same connection (the bug class
// the /validate-ippool handler once had).
func writeAdmissionResponse(w http.ResponseWriter, ar *admissionv1.AdmissionReview, resp *admissionv1.AdmissionResponse) {
	ar.Response = resp

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&ar)
}

func (h *Handler) validateIPPoolAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)

		return
	}

	// the sibling handlers apply the same guard: an AdmissionReview
	// without a request or without an old object cannot be validated, so
	// a well-formed allow is answered instead of dereferencing a nil
	// request (which would panic the handler and, with this entry's
	// implicit failurePolicy Fail, turn every IPPool deletion into an
	// apiserver webhook failure)
	if ar.Request == nil || len(ar.Request.OldObject.Raw) == 0 {
		log.Errorf("the AdmissionReview carries no old object, allowing the request")

		// the response uid must be copied from the request uid: a response
		// whose uid does not match its request is a failed webhook call for
		// the apiserver, which on this entry's implicit failurePolicy Fail
		// would deny the deletion anyway. a request-less review carries no
		// uid to echo
		respUID := types.UID("")
		if ar.Request != nil {
			respUID = ar.Request.UID
		}
		writeAdmissionResponse(w, ar, &admissionv1.AdmissionResponse{
			UID:     respUID,
			Allowed: true,
		})

		return
	}

	pool := &kihv1.IPPool{}
	if err := json.Unmarshal(ar.Request.OldObject.Raw, &pool); err != nil {
		log.Errorf("cannot unmarshal json to pool: %s", err)

		// a corrupt old object cannot be validated; the internal
		// validators of the sibling handlers answer a well-formed allow in
		// the same situation, so a half-written 500 body the apiserver
		// reads as a webhook failure is never produced here
		writeAdmissionResponse(w, ar, &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: true,
		})

		return
	}

	writeAdmissionResponse(w, ar, h.validateIPPool(r.Context(), ar, pool))
}

// evaluateIPPoolSpec returns the sorted problems of an ipv4 configuration
// which cannot serve: a subnet which does not parse as an ipv4 prefix, a
// pool start or pool end outside the subnet, a pool end before its start,
// a pool end or exclude entry equal to the broadcast address of the
// subnet, a pool range larger than the cap, an exclude address outside
// the subnet or the allocation range, and a serverip or router inside the
// allocation range without an exclude entry. the checks mirror the
// controller's own registration validation so a projection the controller
// would register is never rejected: the serverip is otherwise
// parse-checked only, exactly like the controller's projection validation
// (its off-subnet placement stays deliberately allowed). only fields which
// are present are validated.
func evaluateIPPoolSpec(cfg kihv1.IPv4Config) (problems []string) {
	var prefix netip.Prefix
	subnetParses := false
	if cfg.Subnet != "" {
		p, err := netip.ParsePrefix(cfg.Subnet)
		if err != nil || !p.Addr().Is4() {
			problems = append(problems, fmt.Sprintf("the subnet %q does not parse as an ipv4 prefix", cfg.Subnet))
		} else {
			prefix = p
			subnetParses = true
		}
	}

	// the serverip is parse-checked like the controller's own projection
	// validation, but its placement is deliberately not contained to the
	// subnet: the controller registers an off-subnet serverip without a
	// complaint (it only parses the address), so an admission containment
	// check here would reject a projection the controller serves
	if cfg.ServerIP != "" {
		if addr, err := netip.ParseAddr(cfg.ServerIP); err != nil || !addr.Is4() {
			problems = append(problems, fmt.Sprintf("the serverip %q does not parse as an ipv4 address", cfg.ServerIP))
		}
	}

	var rangeStart, rangeEnd netip.Addr
	rangeParses := true
	// rangeRejected records a range problem the controller's own
	// ValidateSubnetSpec rejects the registration for: the infrastructure
	// address check below only fires on a range the controller would
	// actually serve, mirroring the order of the registration validation.
	rangeRejected := false
	if cfg.Pool.Start != "" {
		addr, err := netip.ParseAddr(cfg.Pool.Start)
		if err != nil || !addr.Is4() {
			problems = append(problems, fmt.Sprintf("the pool start %q does not parse as an ipv4 address", cfg.Pool.Start))
			rangeParses = false
		} else {
			rangeStart = addr

			if subnetParses && !prefix.Contains(addr) {
				problems = append(problems, fmt.Sprintf("the pool start %s is not within the subnet %s", cfg.Pool.Start, cfg.Subnet))
				rangeRejected = true
			}
		}
	}

	if cfg.Pool.End != "" {
		addr, err := netip.ParseAddr(cfg.Pool.End)
		if err != nil || !addr.Is4() {
			problems = append(problems, fmt.Sprintf("the pool end %q does not parse as an ipv4 address", cfg.Pool.End))
			rangeParses = false
		} else {
			rangeEnd = addr

			if subnetParses && !prefix.Contains(addr) {
				problems = append(problems, fmt.Sprintf("the pool end %s is not within the subnet %s", cfg.Pool.End, cfg.Subnet))
				rangeRejected = true
			}
		}
	}

	completeRange := rangeParses && cfg.Pool.Start != "" && cfg.Pool.End != ""

	if completeRange && rangeEnd.Compare(rangeStart) < 0 {
		problems = append(problems, fmt.Sprintf("the pool end %s lies before the pool start %s", cfg.Pool.End, cfg.Pool.Start))
		rangeRejected = true
	}

	// the broadcast address of the subnet can neither serve as the pool end
	// nor as an exclude entry, and the range size is capped, exactly like
	// the controller's own registration validation (MaxPoolAddrs)
	var broadcast netip.Addr
	if subnetParses {
		broadcast = ipv4Broadcast(prefix)
	}

	if subnetParses && cfg.Pool.End != "" && rangeParses && rangeEnd == broadcast {
		problems = append(problems, fmt.Sprintf("the pool end %s equals the broadcast address %s of the subnet %s", cfg.Pool.End, broadcast, cfg.Subnet))
		rangeRejected = true
	}

	if completeRange && rangeEnd.Compare(rangeStart) >= 0 && ipv4RangeLen(rangeStart, rangeEnd) > maxPoolAddrs {
		problems = append(problems, fmt.Sprintf("the pool range %s - %s is larger than the maximum of %d addresses", cfg.Pool.Start, cfg.Pool.End, maxPoolAddrs))
		rangeRejected = true
	}

	for _, exclude := range cfg.Pool.Exclude {
		if exclude == "" {
			continue
		}

		addr, err := netip.ParseAddr(exclude)
		if err != nil || !addr.Is4() {
			problems = append(problems, fmt.Sprintf("the exclude address %q does not parse as an ipv4 address", exclude))

			continue
		}

		if subnetParses && !prefix.Contains(addr) {
			problems = append(problems, fmt.Sprintf("the exclude address %s is not within the subnet %s", exclude, cfg.Subnet))
		}

		if subnetParses && addr == broadcast {
			problems = append(problems, fmt.Sprintf("the exclude address %s equals the broadcast address %s of the subnet %s", exclude, broadcast, cfg.Subnet))
		}

		if cfg.Pool.Start != "" && cfg.Pool.End != "" && rangeParses && (addr.Compare(rangeStart) < 0 || addr.Compare(rangeEnd) > 0) {
			problems = append(problems, fmt.Sprintf("the exclude address %s is not within the pool range %s..%s", exclude, cfg.Pool.Start, cfg.Pool.End))
		}
	}

	// the serverip and the router are also checked against the allocation
	// range itself, exactly like the controller's own registration
	// validation (R08): the registration only reserves the exclude
	// entries, so an infrastructure address inside the range must be
	// excluded explicitly or the controller rejects the pool as
	// unregistrable. only a parseable in-range address is reported: an
	// unparseable or off-range entry stays the parse checks' concern and
	// an off-subnet placement stays deliberately allowed, mirroring the
	// controller.
	if completeRange && !rangeRejected {
		for _, infra := range []struct {
			kind  string
			value string
		}{{"serverip", cfg.ServerIP}, {"router", cfg.Router}} {
			if infra.value == "" {
				continue
			}

			addr, addrErr := netip.ParseAddr(infra.value)
			if addrErr != nil || !addr.Is4() {
				continue
			}

			if addr.Compare(rangeStart) < 0 || addr.Compare(rangeEnd) > 0 {
				continue
			}

			excluded := false
			for _, exclude := range cfg.Pool.Exclude {
				if exAddr, exErr := netip.ParseAddr(exclude); exErr == nil && exAddr.Compare(addr) == 0 {
					excluded = true

					break
				}
			}

			if !excluded {
				problems = append(problems, fmt.Sprintf("the %s %s lies within the pool range %s-%s and is not excluded; move it outside the allocation range or add it to the exclude list",
					infra.kind, infra.value, cfg.Pool.Start, cfg.Pool.End))
			}
		}
	}

	sort.Strings(problems)

	return problems
}

// validateIPPoolSpec rejects an IPPool whose ipv4 configuration cannot
// serve. the crd schema accepts spellings the helper controller cannot
// register (for example a subnet length of two digits such as 10.0.0.0/33),
// and an invalid spec which is stored anyway is rejected by the controller
// on every resync - on update the previously registered configuration keeps
// serving while the object carries the broken spec (observed live with an
// exclude address outside the allocation range erroring on every sync until
// it was repaired). denying the write at admission keeps the invalid spec
// out of the cluster entirely.
//
// only fields which are present are validated: an omitted optional field is
// the controller's business, and the checks mirror the controller's own
// registration validation (the subnet, range, broadcast and exclude
// validations, the pool size cap and the parse check of the serverip) so a
// projection the controller would register is never rejected - the
// controller accepts an off-subnet serverip, so the admission check
// deliberately does too. internal failures fail open - the controller's own
// projection rejection stays the authoritative fence.
func (h *Handler) validateIPPoolSpec(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	allow := &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}

	pool := &kihv1.IPPool{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &pool); err != nil {
		log.Errorf("cannot unmarshal json to pool: %s", err)

		return allow
	}

	problems := evaluateIPPoolSpec(pool.Spec.IPv4Config)

	if len(problems) == 0 {
		return allow
	}

	log.Warnf("(service.validateIPPoolSpec) denying the %s of IPPool %s: %s",
		strings.ToLower(string(ar.Request.Operation)), pool.Name, strings.Join(problems, "; "))

	return &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: false,
		Result: &metav1.Status{
			Message: fmt.Sprintf("the ipv4 configuration of the IPPool cannot serve: %s (the helper controller would reject this projection on its own sync)", strings.Join(problems, "; ")),
		},
	}
}

func (h *Handler) validateIPPoolSpecAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)

		return
	}

	if ar.Request == nil || len(ar.Request.Object.Raw) == 0 {
		log.Errorf("the AdmissionReview carries no object, allowing the request")

		// the response uid must be copied from the request uid: a response
		// whose uid does not match its request is a failed webhook call
		// for the apiserver. a request-less review carries no uid to echo
		respUID := types.UID("")
		if ar.Request != nil {
			respUID = ar.Request.UID
		}
		writeAdmissionResponse(w, ar, &admissionv1.AdmissionResponse{
			UID:     respUID,
			Allowed: true,
		})

		return
	}

	writeAdmissionResponse(w, ar, h.validateIPPoolSpec(ar))
}

func (h *Handler) validateVmNetCfgAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)

		return
	}

	if ar.Request == nil || len(ar.Request.Object.Raw) == 0 {
		log.Errorf("the AdmissionReview carries no object, allowing the request")

		// the response uid must be copied from the request uid: a response
		// whose uid does not match its request is a failed webhook call
		// for the apiserver. a request-less review carries no uid to echo
		respUID := types.UID("")
		if ar.Request != nil {
			respUID = ar.Request.UID
		}
		writeAdmissionResponse(w, ar, &admissionv1.AdmissionResponse{
			UID:     respUID,
			Allowed: true,
		})

		return
	}

	writeAdmissionResponse(w, ar, h.validateVmNetCfg(r.Context(), ar))
}

func (h *Handler) Run() {
	homedir := os.Getenv("HOME")
	keyPath := fmt.Sprintf("%s/tls.key", homedir)
	certPath := fmt.Sprintf("%s/tls.crt", homedir)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/validate-ippool", h.validateIPPoolAdmission)
	mux.HandleFunc("/validate-ippool-spec", h.validateIPPoolSpecAdmission)
	mux.HandleFunc("/validate-vmnetcfg", h.validateVmNetCfgAdmission)
	mux.HandleFunc("/validate-vm", h.validateVirtualMachineAdmission)

	h.httpServer = &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
	}

	// a fatal listen error (missing/corrupt tls data, port bind failure)
	// must kill the process: returning would leave a webhook serving
	// nothing, which no probe currently detects and which hangs every
	// ippool deletion until the apiserver times out
	if err := h.httpServer.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
		log.Fatalf("(webhook.service) %s", err.Error())
	}
}

// httpDrainBudget bounds the graceful drain of the admission server
// (F08): Shutdown waits for the in-flight admission requests, and the
// process context it used to run on has no deadline, so a drain behind a
// stalled connection wedged the renewal restart which calls Stop
// synchronously. the admission requests are bounded by the apiserver's
// webhook timeout through their propagated request contexts, so a drain
// which cannot complete within this budget is a stalled connection, not
// a legitimately slow admission. it is a variable so the test can shrink
// it. the 25s sit below the 30s termination grace period of the
// deployments (F14): the process's own bounded exit must win the race
// against kubelet's kill at the grace deadline, or a stalled drain ends
// in a SIGKILL instead of the clean exit of the shutdown path.
var httpDrainBudget = 25 * time.Second

func (h *Handler) Stop() error {
	drainCtx, cancel := context.WithTimeout(context.Background(), httpDrainBudget)
	defer cancel()

	return h.httpServer.Shutdown(drainCtx)
}

// staticIPInterfaceNetwork resolves the canonical network of the vm
// interface which the static ip annotation names. the first problem is
// returned as the denial message: a vm template without interfaces, an
// interface name the vm does not define, an interface without a network of
// the same name, a network which is not a multus network, a multus
// network without a networkname, and a networkname which is not a valid
// network reference. only multus networks can carry a reservation: the
// controller only configures the multus interfaces of a vm.
func staticIPInterfaceNetwork(vm *kubevirtV1.VirtualMachine, nicName string) (network string, denied *string) {
	if vm.Spec.Template == nil {
		msg := fmt.Sprintf("the static ip annotation requests interface %s, but the vm template defines no interfaces", nicName)

		return "", &msg
	}

	defined := false
	for _, nic := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		if nic.Name == nicName {
			defined = true

			break
		}
	}

	if !defined {
		msg := fmt.Sprintf("the static ip annotation requests interface %s, which the vm does not define in spec.template.spec.domain.devices.interfaces", nicName)

		return "", &msg
	}

	for _, net := range vm.Spec.Template.Spec.Networks {
		if net.Name != nicName {
			continue
		}

		if net.Multus == nil {
			msg := fmt.Sprintf("the static ip annotation requests interface %s, whose network is not a multus network: only multus networks carry a reservation", nicName)

			return "", &msg
		}

		if net.Multus.NetworkName == "" {
			msg := fmt.Sprintf("the static ip annotation requests interface %s, whose multus network carries no networkname", nicName)

			return "", &msg
		}

		network = util.QualifyNetworkName(vm.Namespace, net.Multus.NetworkName)
		if network == "" {
			msg := fmt.Sprintf("the static ip annotation requests interface %s, whose networkname %q is not a valid network reference", nicName, net.Multus.NetworkName)

			return "", &msg
		}

		return network, nil
	}

	msg := fmt.Sprintf("the static ip annotation requests interface %s, which has no network in spec.template.spec.networks", nicName)

	return "", &msg
}

// checkStaticIPAddress rejects a requested static address which the IPPool
// of its interface cannot serve for this vm: the broadcast address of the
// pool subnet, an address outside the pool range (mirroring the vmnetcfg
// range guard and the controller's own registration validation), an excluded
// address, an address recorded in the pool status under another owner, and
// an address reserved by an exclude. the owner is compared on its namespace
// and vm name only, so a vm which kept its address after its macaddress
// changed keeps it: the helper keys the pool status on the (vm, macaddress)
// pair, and the controller's reclaim accepts the same vm through its
// claimant vmRef.
//
// internal failures fail open exactly like the vmnetcfg checks: a pool whose
// range or subnet does not parse and an unparseable recorded owner never
// deny, because the controller's own claim stays the authoritative guard.
func checkStaticIPAddress(vm *kubevirtV1.VirtualMachine, nicName string, address string, pool *kihv1.IPPool) (denied *string) {
	ip, err := netip.ParseAddr(address)
	if err != nil || !ip.Is4() {
		msg := fmt.Sprintf("the static ip address %s of interface %s does not parse as an ipv4 address (IPPool %s)",
			address, nicName, pool.Name)

		return &msg
	}

	// the broadcast address of the subnet is named as such before the
	// range check: a range which reaches it (a pool end equal to the
	// broadcast, which the ippool guard rejects) would otherwise report
	// the less precise range problem
	if prefix, prefixErr := netip.ParsePrefix(pool.Spec.IPv4Config.Subnet); prefixErr == nil && prefix.Addr().Is4() && ip == ipv4Broadcast(prefix) {
		msg := fmt.Sprintf("the static ip address %s of interface %s is the broadcast address %s of the subnet %s (IPPool %s): no interface can hold it",
			address, nicName, ip, pool.Spec.IPv4Config.Subnet, pool.Name)

		return &msg
	}

	start, startErr := netip.ParseAddr(pool.Spec.IPv4Config.Pool.Start)
	end, endErr := netip.ParseAddr(pool.Spec.IPv4Config.Pool.End)
	if startErr == nil && endErr == nil && start.Is4() && end.Is4() && (ip.Compare(start) < 0 || ip.Compare(end) > 0) {
		msg := fmt.Sprintf("the static ip address %s of interface %s is not between the pool range %s..%s of network %s (IPPool %s)",
			address, nicName, pool.Spec.IPv4Config.Pool.Start, pool.Spec.IPv4Config.Pool.End, pool.Spec.NetworkName, pool.Name)

		return &msg
	}

	for _, exclude := range pool.Spec.IPv4Config.Pool.Exclude {
		excluded, excludeErr := netip.ParseAddr(exclude)
		if excludeErr != nil || !excluded.Is4() {
			continue
		}

		if excluded == ip {
			msg := fmt.Sprintf("the static ip address %s of interface %s is excluded by the pool range %s..%s of network %s (IPPool %s)",
				address, nicName, pool.Spec.IPv4Config.Pool.Start, pool.Spec.IPv4Config.Pool.End, pool.Spec.NetworkName, pool.Name)

			return &msg
		}
	}

	owner, claimed := pool.Status.IPv4.Allocated[address]
	if !claimed {
		return nil
	}

	if owner == kihipam.ExcludedOwner {
		msg := fmt.Sprintf("the static ip address %s of interface %s is a reserved exclude of network %s (IPPool %s)",
			address, nicName, pool.Spec.NetworkName, pool.Name)

		return &msg
	}

	// the owner is compared on its namespace and vm name only: the
	// controller's reclaim accepts the same vm whatever macaddress its
	// interfaces carry, so a vm which changed its macaddress keeps its
	// address. an unparseable owner is unprovable and fails open.
	namespace, vmName, _, ok := util.ParseAllocationRef(owner)
	if ok && (namespace != vm.Namespace || vmName != vm.Name) {
		msg := fmt.Sprintf("the static ip address %s of interface %s is already allocated to %s of network %s (IPPool %s)",
			address, nicName, owner, pool.Spec.NetworkName, pool.Name)

		return &msg
	}

	return nil
}

// validateVirtualMachineStaticIPs rejects every requested address of a vm
// which its IPPools cannot serve for it: an unknown or non-multus
// interface, a network no IPPool serves, an address the pool of that
// network cannot hand out, and an address two interfaces of the same vm
// request at once. the requests are checked in a deterministic order, so the
// denial of a vm with several problems is stable.
//
// a failed IPPool list fails open like the vmnetcfg checks: the
// controller's own claim stays the authoritative guard.
func (h *Handler) validateVirtualMachineStaticIPs(ctx context.Context, vm *kubevirtV1.VirtualMachine, requested map[string]string) (denied *string) {
	pools, err := h.listIPPools(ctx)
	if err != nil {
		log.Errorf("(service.validateVirtualMachineStaticIPs) cannot list the IPPools, allowing the request: %s", err.Error())

		return nil
	}

	poolByNetwork := ipPoolByNetwork(pools)

	nicNames := make([]string, 0, len(requested))
	for nicName := range requested {
		nicNames = append(nicNames, nicName)
	}
	sort.Strings(nicNames)

	claimed := map[string]string{}
	for _, nicName := range nicNames {
		network, msg := staticIPInterfaceNetwork(vm, nicName)
		if msg != nil {
			return msg
		}

		pool := poolByNetwork[network]
		if pool == nil {
			msg := fmt.Sprintf("the static ip annotation requests address %s on interface %s, but no IPPool serves its network %s",
				requested[nicName], nicName, network)

			return &msg
		}

		if msg := checkStaticIPAddress(vm, nicName, requested[nicName], pool); msg != nil {
			return msg
		}

		if otherNic, duplicate := claimed[requested[nicName]]; duplicate {
			msg := fmt.Sprintf("the static ip annotation requests address %s on both interfaces %s and %s",
				requested[nicName], otherNic, nicName)

			return &msg
		}

		claimed[requested[nicName]] = nicName
	}

	return nil
}

// validateVirtualMachine rejects a VirtualMachine whose static ip annotation
// requests an address the helper cannot reserve for it: a malformed
// annotation, an interface the vm does not define or whose network is not a
// multus network, a network without an IPPool, and an address which the pool
// cannot serve (outside its range, its broadcast address, an exclude entry, a
// reservation of another vm, or an address two of its interfaces request).
// the reservation itself stays check-at-admission and claim-at-reconcile: the
// vm controller claims the address through the existing ownership-checked ipam
// path, so this gate only keeps a request out of the cluster which could
// never be served.
//
// a vm without the annotation, an annotation without any address and an
// address a pool cannot range-check are all admitted, and internal lookup
// failures fail open, exactly like the vmnetcfg checks: the controller stays
// the authoritative guard.
func (h *Handler) validateVirtualMachine(ctx context.Context, ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	allow := &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}

	vm := &kubevirtV1.VirtualMachine{}
	if err := json.Unmarshal(ar.Request.Object.Raw, vm); err != nil {
		log.Errorf("cannot unmarshal json to virtualmachine: %s", err)

		return allow
	}

	requested, err := util.ParseStaticIPAnnotation(vm.ObjectMeta.Annotations)
	if err != nil {
		log.Warnf("(service.validateVirtualMachine) denying VirtualMachine %s/%s: %s",
			vm.Namespace, vm.Name, err.Error())

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	if len(requested) == 0 {
		return allow
	}

	if msg := h.validateVirtualMachineStaticIPs(ctx, vm, requested); msg != nil {
		log.Warnf("(service.validateVirtualMachine) denying VirtualMachine %s/%s: %s",
			vm.Namespace, vm.Name, *msg)

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: *msg,
			},
		}
	}

	return allow
}

func (h *Handler) validateVirtualMachineAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)

		return
	}

	if ar.Request == nil || len(ar.Request.Object.Raw) == 0 {
		log.Errorf("the AdmissionReview carries no object, allowing the request")

		w.Header().Set("Content-Type", "application/json")
		ar.Response = &admissionv1.AdmissionResponse{
			UID:     "",
			Allowed: true,
		}
		json.NewEncoder(w).Encode(&ar)

		return
	}

	writeAdmissionResponse(w, ar, h.validateVirtualMachine(r.Context(), ar))
}
