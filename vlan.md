# Multi-network DHCP: implementation and operating contract

## 1. What this change delivers

Run one kubevirt-ip-helper Deployment for each network, with one helper pod
by default. Each Deployment serves one NetworkAttachmentDefinition (NAD),
elects its own leader, and runs DHCP only for that network. Additional replicas
are optional high availability for the same network.

A network is identified by **NAD namespace + NAD name**, not by a VLAN number.
The NAD owns the VLAN configuration; the CNI creates the pod interface. The
helper uses that interface without creating or configuring VLAN devices.
Tagged and untagged NADs follow the same rules.

The helper already has interface-bound DHCP servers, IP allocation, durable
reservation records, and leader election. This change adds **isolation between
network-specific deployments**, **safe cooperation when one VM uses several
networks** and the **VM static-ip annotation** of section 5, which requests a
specific address for one NIC. It does not replace the DHCP implementation.

The implementation isolates network-specific deployments and merges their
updates to shared VMNetCfgs without replacing foreign state. The runtime,
packaging and E2E harness follow the contracts below. E2E and live-cluster
qualification have not been performed for this implementation; the acceptance
matrix remains the release qualification contract, not a claim of live results.

### Deployment layout

All helper Deployments and NADs live centrally in the infrastructure namespace
`kubevirt-ip-helper`. Install one chart release covering all served networks;
the examples name it `kubevirt-ip-helper`. Plain YAML supports the same topology.
There is one NAD per served network, not per VM namespace; tenant VMs in other
namespaces share it using namespace-qualified network references.

```text
Infrastructure namespace: kubevirt-ip-helper
  helper-management (1 pod)          helper-storage (1 pod)
    Lease: ...-management             Lease: ...-storage
    Service: metrics-management       Service: metrics-storage
    pod interface: net1               pod interface: net1
             |                                 |
    NAD: management                   NAD: storage
             |                                 |
-------------|---------------------------------|-----------------
Tenant namespace: tenant-a                     |
             | VM NIC A                        | VM NIC B
             +----------------+----------------+
                              |
                        VM + VMNetCfg
```

Admission is one shared cluster-wide service, not one webhook per network.
The merged webhook pins Service `kubevirt-ip-helper-webhook` in namespace
`kubevirt-ip-helper`, Service port `8080`, listener `8443`, and
ValidatingWebhookConfiguration `kubevirt-ip-helper-validator`. Its TLS Secret
is `kubevirt-ip-helper-webhook-tls`; its CSR and serving DNS identity derive
from `kubevirt-ip-helper-webhook.kubevirt-ip-helper.svc`. The chart explicitly
sets `webhook.fullnameOverride: kubevirt-ip-helper-webhook` to match this
runtime identity. This canonical naming is deliberate; arbitrary webhook
names/namespaces and new webhook configuration flags are outside this design.

The two `net1` interfaces are in different pods. Their identical names do not
make them the same network. Each network must represent a distinct DHCP
broadcast domain; two NAD names for the same segment must not be deployed as
independent, competing DHCP authorities.

**Supported topology:** one served NAD per helper Deployment; any number of
networks per VM. Serving several NADs from one helper Deployment is not part
of this design.

### Behavior across the migration

| Area | Before migration | Current implementation |
| --- | --- | --- |
| Pool discovery | Lists and watches all IPPools | Discovers only pools labelled for this helper's network |
| Leadership | One fixed Lease name | One derived Lease name per network |
| VMNetCfg reconciliation | Rebuilds a shared object's entries without a network boundary | Changes only this network's NIC entries and merges concurrent writes |
| VMNetCfg deletion | One helper cleans the entire object | Each network cleans its entries; the last completion releases the object |
| Metrics routing | One leader-selected Service | One leader-selected Service per network |
| VMNetCfg admission | CREATE and main-resource UPDATE validate every remaining NIC row | CREATE validates all rows; UPDATE validates added/modified rows against `OldObject`, preserving untouched foreign rows |
| Packaging | One helper Deployment per chart release; webhook included in each release | One release generates all network helper/metrics pairs and retains shared singleton resources |
| VLAN creation and DHCP protocol | CNI provides the interface; DHCP binds to it | Unchanged |

## 2. Network identity and configuration

### Required labels

| Object | Label | Value |
| --- | --- | --- |
| Helper pod | `kubevirtiphelper/network` | NAD name, for example `management` |
| IPPool | `kubevirtiphelper/network` | The same NAD name |
| IPPool | `kubevirtiphelper/network-namespace` | NAD namespace: `kubevirt-ip-helper` |

Every helper runs in the shared infrastructure namespace with its served NAD.
This is a deployment constraint of this design, not a Multus restriction.
VMs run in their own namespaces and reference `kubevirt-ip-helper/management`.
If Multus namespace isolation is enabled, its policy must permit tenant VMs
to reference NADs in this shared namespace. This is an operator configuration
prerequisite, not a change to the CNI implementation.

The helper reads its own Pod object using the existing pod-name and namespace
lookup. It takes the NAD name from the required pod label and constructs:

```text
pod namespace:        kubevirt-ip-helper
pod network label:    management
canonical network:    kubevirt-ip-helper/management
pool selector:        kubevirtiphelper/network=management,
                      kubevirtiphelper/network-namespace=kubevirt-ip-helper
leader Lease:         kubevirt-ip-helper-lock-management
Lease namespace:      kubevirt-ip-helper
```

The second pool label is necessary because IPPools are cluster-scoped. Two
namespaces can both contain a NAD called `management`; a name-only selector would
mix their pools. The helper needs no second pod label: its namespace already
supplies that value.

### Configuration rules

- The pod label must contain a nonempty DNS-label NAD name, at most 63
  characters. Missing or invalid identity prevents startup and leader election.
- Every selected pool must have `spec.networkname` equal to the canonical
  network. Validate this before changing addresses, reservations or listeners.
- Pool network references are fully qualified. For VM and VMNetCfg NIC
  comparisons, an unqualified reference resolves in the containing object's
  namespace; newly projected VM NIC entries use the qualified form. Do not
  rewrite another network's stored entries.
- Identity is fixed for the lifetime of a helper process. Moving a helper to a
  different network requires replacement pods, not a live label edit.
- The deployment's Multus attachment must agree with its declared network.
  Interface existence alone does not prove network membership. The helper does
  not infer its identity from Multus status or fall back to environment values.
- A live pool must not be moved between networks by relabelling it. Drain it
  and recreate it for the destination network.

Use Kubernetes **label selectors**, not selectors on `spec.networkname`.
This works with the existing CRDs and supported Kubernetes versions; no
`selectableFields` or other schema changes are required.

### Resource names

The Lease name is derived in Go as `kubevirt-ip-helper-lock-<nad-name>`.
Its namespace separates identically named networks in different namespaces.
The maximum derived length is 87 characters, within the Lease object's
253-character DNS-subdomain limit.

Deployment and metrics Service names are explicit, required per-network
manifest/chart inputs, not truncated concatenations of release and NAD names.
Require valid DNS-label names of at most 63 characters and uniqueness among
resources of the same kind, including shared resources. NAD names must also
be unique within the network list. Reject invalid or duplicate values rather
than silently truncating names into collisions.

## 3. Configuration example

For the management network, use a NAD named `management` in namespace
`kubevirt-ip-helper`. Its CNI configuration attaches the helper interface as
`net1`; any VLAN tagging is configured separately in the NAD. Host bridge/trunk
connectivity remains an operator prerequisite.

### Plain YAML

The following Deployment excerpt shows the network-specific fields. Retain
the existing container, shared helper ServiceAccount, security context,
probes and lifecycle settings. New per-network Deployments use disjoint
selectors matching their templates. Plain YAML keeps its existing `app`
label convention; the chart keeps its own release labels as described below.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helper-management
  namespace: kubevirt-ip-helper
spec:
  replicas: 1
  selector:
    matchLabels:
      app: kubevirt-ip-helper
      kubevirtiphelper/network: management
  template:
    metadata:
      labels:
        app: kubevirt-ip-helper
        kubevirtiphelper/network: management
      annotations:
        k8s.v1.cni.cncf.io/networks: '[{"name":"management","namespace":"kubevirt-ip-helper","interface":"net1"}]'
```

The matching pool retains the existing IPv4 configuration format:

```yaml
apiVersion: kubevirtiphelper.k8s.binbash.org/v1
kind: IPPool
metadata:
  name: pool-management
  labels:
    kubevirtiphelper/network: management
    kubevirtiphelper/network-namespace: kubevirt-ip-helper
spec:
  networkname: kubevirt-ip-helper/management
  bindinterface: net1
  ipv4config:
    serverip: 10.100.0.2
    subnet: 10.100.0.0/24
    pool:
      start: 10.100.0.100
      end: 10.100.0.110
    router: 10.100.0.1
    dns: ["10.100.0.1"]
    domainname: management.example.test
    leasetime: 600
```

Its metrics Service selects only this network's active leader. Retaining the
Service's `app` label lets the plain-YAML ServiceMonitor discover it.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: metrics-management
  namespace: kubevirt-ip-helper
  labels:
    app: kubevirt-ip-helper
    kubevirtiphelper/network: management
spec:
  selector:
    app: kubevirt-ip-helper
    kubevirtiphelper/network: management
    kubevirtiphelper/leader: active
  ports:
    - name: metrics
      port: 8080
      targetPort: 8080
      protocol: TCP
```

Storage gets a second NAD, Deployment, pool and Service with `storage` and its
own IPv4 settings. Both networks share the infrastructure namespace and helper
ServiceAccount/RBAC. NADs and IPPools are operator-managed: the chart references
them and does not create duplicate network or pool objects.

### Tenant VM reference

This VM excerpt places both NICs in `tenant-a` while using the shared NADs.
VMs in `tenant-b` use the same qualified references, not copies of the NADs.
The VM's one VMNetCfg remains in the VM's namespace.

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: example-vm
  namespace: tenant-a
spec:
  template:
    spec:
      domain:
        devices:
          interfaces:
            - name: nic-a
              macAddress: "02:00:00:00:01:01"
              bridge: {}
            - name: nic-b
              macAddress: "02:00:00:00:02:01"
              bridge: {}
      networks:
        - name: nic-a
          multus:
            networkName: kubevirt-ip-helper/management
        - name: nic-b
          multus:
            networkName: kubevirt-ip-helper/storage
```

### Helm values and rendered resources

One release `kubevirt-ip-helper` in namespace `kubevirt-ip-helper` uses:

```yaml
kubevirtiphelper:
  networks:
    - name: management
      interface: net1
      deploymentName: helper-management
      metricsServiceName: metrics-management
      replicaCount: 1
    - name: storage
      interface: net1
      deploymentName: helper-storage
      metricsServiceName: metrics-storage
      replicaCount: 1
  serviceMonitor:
    enabled: true
webhook:
  fullnameOverride: kubevirt-ip-helper-webhook
  service:
    webhookServicePort: 8080
    webhookListenPort: 8443
```

Each `kubevirtiphelper.networks` entry requires `name` (the NAD name),
`interface`, `deploymentName` and `metricsServiceName`. Its `replicaCount`
defaults to one, permits zero for staged cutover, and permits extra replicas
for optional HA. Preserve an explicit zero rather than treating it as a missing
value. The chart uses this per-entry setting instead of the old global helper
`replicaCount`; image, security, resources and scheduling settings remain shared.

Each entry renders exactly one helper Deployment and one metrics Service.
The one-NAD Multus annotation is derived from the entry's `name`, `interface`
and release namespace. Global `podAnnotations` cannot supply a fallback or
override that attachment. Other shared pod annotations remain available.

Chart selectors retain `app.kubernetes.io/name` and
`app.kubernetes.io/instance`; plain manifests retain `app`. Both methods add
`kubevirtiphelper/network` to helper pod labels, Deployment selectors and
metrics Service labels/selectors. Metrics selectors additionally require
`kubevirtiphelper/leader: active`. Do not mix the two label conventions.
One chart ServiceMonitor selects every network's metrics Service through
the common release labels and endpoint port name `metrics`, without a
single-network selector.

The helper ServiceAccount and its RBAC, the webhook Deployment/Service and
its ServiceAccount/RBAC, TLS management and admission registration remain
singleton resources. Do not put them inside the network loop. The single
ServiceMonitor is optional: `kubevirtiphelper.serviceMonitor.enabled: false`
omits it on clusters without the monitoring CRD. The plain manifest separates
it into `deployments/servicemonitor.yaml`; apply that optional file only when
the monitoring CRD is available.

Chart and plain helper manifests use the same probes and lifecycle settings:

| Setting | Chart and plain helper behavior |
| --- | --- |
| Startup probe | `/healthz`, port 8080, period 10s, failure threshold 90 |
| Liveness probe | `/healthz`, port 8080, initial delay 15s, period 10s, timeout 3s, failure threshold 3 |
| Readiness probe | `/ready`, port 8080, initial delay 5s, period 10s, timeout 3s, failure threshold 3 |
| Deployment lifecycle | Progress deadline 1800s, revision history 10, RollingUpdate with maxSurge 1 and maxUnavailable 0 |
| Pod lifecycle | Termination grace period 30s; preserve the existing signal-driven drain and Lease release |

Readiness does not require leadership: a healthy standby is ready without
serving DHCP. The leader label, not readiness alone, routes metrics.

## 4. Startup, discovery and pool lifetime

`app.Init` resolves and validates the own-pod network identity before
constructing the Lease lock or performing startup network cleanup. The handler
stores one immutable `util.NetworkScope` and passes it to the controllers.

```text
Read own Pod and required label
             |
             v
Build canonical network, selector and Lease name
             |
             v
Startup network cleanup: selected pools only
             |
             v
Join this network's leader election
             |
        +----+----+
        |         |
     standby    leader
                  |
                  v
        LIST selected IPPools -> record pool-gate snapshot
                  |
                  v
        Start selected pool LIST/WATCH; register own pools
                  |
                  v
        Replay VMNetCfgs, processing own NICs only
                  |
                  v
        Complete existing startup gates -> run services
```

The initial pool LIST, the IPPool watcher's LIST/WATCH, and startup network
cleanup all use the same two-label selector. Pools outside that result never
enter the local pool cache, registration path or pool startup gate.

The existing registration sequence remains: validate pool configuration,
find `bindinterface`, add the server address, and start the interface-bound
DHCP server. No VLAN device is created. A matching pool with a missing
interface fails registration; it is not silently ignored. A selected pool
whose label and `spec.networkname` disagree likewise fails registration.

An empty selected pool list is valid: the pool gate is empty, and later pool
creation is handled by the watcher. It does not bypass the VMNetCfg gate.
Own-NIC restore failures retain the existing settlement/retry rules. A
stalled startup retains the existing 15-minute no-progress limit. Standby
health and readiness behavior remain unchanged.

### Discovery is not proof of deletion

A pool can disappear from a label-filtered watcher because its label changed,
not because the object was deleted. Handle that distinction explicitly:

1. On a disappearance event, read the pool by its object name.
2. `NotFound` confirms deletion. An API error requires retry.
3. If the object still exists but no longer matches, stop its local service
   and remove its local registration without discarding its durable reservation
   ledger. Report the configuration mismatch; restoring the labels allows replay.

The existing cache-miss existence checks in the VM and VMNetCfg cleanup paths
remain **unfiltered, one-shot API checks**, reached only for an owned NIC.
They establish whether a pool with that network still exists, including one
whose labels are missing. A live but unavailable pool blocks cleanup rather
than permitting its ledger entry to be abandoned.

This is a deliberate exception to scoped discovery: a verification response
can contain foreign pool objects, but none is registered, cached or reconciled.
Never feed these responses into a startup gate. Never interpret “outside the
selector” as “deleted”.

## 5. VMs spanning several networks

Keep one VMNetCfg per VM. Its `spec.networkconfig` and
`status.networkconfig` arrays contain NIC entries from all of the VM's
networks. No new labels are required on VMs or VMNetCfgs.

Both object watchers and the VMNetCfg startup LIST remain unfiltered. A
helper's ownership boundary is an individual NIC entry, identified by
**network name and MAC address**, not the whole object.

```text
VMNetCfg in tenant-a for one VM
  NIC A: kubevirt-ip-helper/management
    -> helper-management: may change A, must preserve B
  NIC B: kubevirt-ip-helper/storage
    -> helper-storage: may change B, must preserve A
```

Apply the ownership check before allocation, restore, NIC removal, rollback,
lease deletion, ledger updates, status/error projection and metrics updates.
Preserve foreign spec/status entries, including entries present in only one
array, with their values and relative ordering unchanged.

A foreign-only object with no pending local cleanup is a no-op: it settles
this helper's startup gate without creating a VMNetCfg or writing spec or
status. A mixed object settles according to this helper's own NIC work.
Startup replay uses exactly the same partition as steady-state reconciliation.
VM deletion and orphan detection may initiate deletion of a shared VMNetCfg
only under the existing parent/UID checks; completing that deletion follows
section 6. A foreign-only object does not trigger such writes from this helper.

### Creating and updating the shared object

The VM controller projects only its network's NICs. It creates a VMNetCfg only
when it has owned NICs to represent. If another helper wins creation, handle
`AlreadyExists` by reading the object and merging the owned entries; never
replace the winner's entries.

Both controllers must use the same merge discipline for subsequent writes:

1. Read the current object from the API, not just an informer snapshot.
2. Verify UID, deletion state, and the owned entries on which the operation
   was based. A stale owned-entry decision must be reconciled again, not
   forced over a newer change.
3. Replace only the owned subset. Preserve foreign entries and metadata.
   The VM controller must also preserve allocated addresses in owned spec
   entries when only projecting the VM's desired interfaces.
4. Write with the current `resourceVersion`; on conflict, read and merge
   again using a bounded, context-aware retry. Skip unchanged writes.

The CRD has a status subresource: spec/metadata use `Update`, while status
uses `UpdateStatus`. These are separate writes and require separate merges.

Do not repeat IP allocation, lease creation or ledger mutations inside the
API conflict loop. Rebase only the intended API update. If the object was
replaced, deleted or changed so the allocation is no longer wanted, use the
existing owner-checked unwind path. Exhausted retries return to the existing
reconciliation/error handling; they do not justify overwriting live data.

A lost API response does not prove that a write failed to commit. Fresh-state
verification preserves an owner-matching served binding when the API still
has the baseline row or already has the exact intended allocated-IP row.
A different changed owned IP is not accepted as that commit. Ambiguous-write
recovery therefore preserves valid ownership without authorizing stale writes
or deleting a successfully committed binding.

### Static IP requests

A VM can request an address for one of its interfaces with the
`kubevirtiphelper.k8s.binbash.org/static-ip` annotation on the VirtualMachine's
own `metadata.annotations`, not on the template. Its value is a json object of
interface name to ipv4 address, the same key space the
`harvesterhci.io/mac-address` annotation already uses:

```yaml
metadata:
  annotations:
    kubevirtiphelper.k8s.binbash.org/static-ip: '{"nic-a":"172.16.0.50"}'
```

Every key must be a `spec.template.spec.domain.devices.interfaces[].name` of
that VM whose `networks[]` entry is a Multus network. The request crosses three
layers, and only the ledger layer gains a write: the annotation is read, the row is
projected by the existing VMNetCfg write path, and the release reuses the existing
owner-checked cleanup.

| Layer | What the request does |
| --- | --- |
| VirtualMachine `metadata.annotations` | The request itself. Each helper honors the entries of the interfaces it serves; an entry for a NIC on a network another helper serves is silently left to that helper, with no warning. Removing the entry releases the address and returns that interface to a dynamic address. |
| VMNetCfg `spec.networkconfig[]` | The owned row of that NIC carries the requested address in `ipaddress`; that address is claimed for the row's `(qualified network, canonical MAC)` identity. The request wins over an address already recorded in that row; no new CRD field is added. |
| IPPool `status.ipv4.allocated` | The durable claim the address is reserved in. Admission reads it; the controller writes it through the existing `ReclaimIPClaimant` and allocation path. |

Reservation stays **check at admission, claim at reconcile**: admission performs
no ledger write and the allocator is unchanged. The release of a removed annotation
is driven by the helper's reconcile, and its trigger lives in the helper's memory
like the existing pending ledger unwinds: a helper restart between the removal and
that reconcile keeps the stored address served, with no breakage and no release,
until the next annotation change or NIC removal. The release is not durable in the
CRD.

The declaration is an allocation-time exclusion, not only an admission check. The
VMNetCfg reconcile reads the static-ip annotations of the cluster's
VirtualMachines before it allocates - one cluster-wide VirtualMachine LIST per
network and reconcile, memoized across the nics of the object - and its fresh
allocation skips every address those annotations declare for the network. A NIC
whose VMNetCfg row carries no address while its VM declares one for that
interface claims exactly the declared address, through the existing
`ReclaimIPClaimant`, instead of taking a dynamic one; a NIC which asks for
nothing is never served an address some other VM declared. The claim stays
owner-validated, so a declared address which another owner already holds is
refused with the same ERROR status and steady-state retry as any other refused
claim, and the declaring NIC's own held claim of an earlier failed sync is
reclaimed idempotently. If the declaration read fails, the helper logs the
failure and allocates without the exclusion for that reconciliation - the
pre-existing behavior, so a transient API read never fails an object whose other
interfaces still restore. The next resync repeats the read, and a declared
address which was taken in the meantime converges through the declaring NIC's
ERROR-and-retry path. The exclusion is allocator-side only
(`AllocateIPExcluding`); the DHCP handler still answers every request from the
lease the ledger records, and the plain `AllocateIP` keeps its semantics for
callers without declarations.

Adding the annotation to a running VM changes the ledger immediately but not the
guest: the reconcile records the declared address for that NIC (the previously
served address is released), while the guest keeps the address it holds until its
next DHCP request, up to its lease time, and the helper answers that request from
the reservation. The ledger already names the declared address for the NIC in
that window; only the guest's own interface still carries the old one.

#### Admission boundary

Admission decides against the durable IPPool ledger only. It does not read the
helper's in-memory allocator or DHCP lease state, so two VMs admitted in the same
moment can both request the same free address and both pass. The loser's interface
then records a sticky ERROR status at reconcile, until the address is freed. An
address the VM itself already holds is accepted: the record is matched on namespace
and vmname only, so a changed macaddress keeps the address.

A VM is rejected when the annotation does not parse as a json object of interface
name to ipv4 address, when a key names no interface of that VM, when that
interface's `networks[]` entry is missing, not Multus, or has an empty
`networkName`, when the NIC's network has no IPPool, or when the requested
address cannot be served by that pool: outside the allocation range, an exclude
entry, the subnet's broadcast address, recorded for another VM in the durable
status, or requested twice inside one VM. This is deliberately stricter than
the VMNetCfg row rule, which skips its range check while the pool is absent: the
annotation is an explicit request, so a VM created before its pool is rejected
instead of admitted into an ERROR-then-recover path. A VM without the annotation
is never rejected by this check.

#### Deliberate fail-open/fail-closed split

The webhook and the controller fail in opposite directions on purpose:

- The webhook fails **closed** on a malformed annotation and on an address the pool
  cannot serve, because a wrong reservation is expensive to discover later. It fails
  **open** where it cannot decide at all: the entry is `failurePolicy: Ignore`, a
  failed pool list or an unparseable pool range admits the VM, and the controller
  remains authoritative.
- The controller fails **soft** on a malformed annotation and on a lookup error. A
  malformed annotation or a key naming an interface the VM does not have is logged
  as a warning and ignored, so a hand-edited VM never wedges the projection of its
  other interfaces; an entry for a NIC on a network this helper does not serve is
  silently not projected, with no warning, because another helper owns it. An IPPool
  lookup or API error leaves that NIC with an ERROR status and a deferred retry
  while the VM's other interfaces still project, instead of completing the
  projection against stale state.

The pre-existing `spec.networkconfig[].ipaddress` field keeps its claim-or-ERROR
behavior: it is claimed for its VM and MAC, or refused by the owning helper with
an ERROR status. The annotation adds a request path in front of it, not a
replacement.

### Admission compatibility

`pkg/webhook/service/service.go` validates VMNetCfg updates as a delta.
An unchanged invalid NIC on B does not block A's allocation commit, spec-row
removal or metadata/finalizer acknowledgement. Conflict retries cannot fix
an explicit admission denial, so the validation boundary is:

- CREATE validates every new row.
- UPDATE compares with `OldObject` and validates only added or modified rows.
  Match unchanged rows by their complete stored content, not their array
  position, and account for duplicates so inserting an extra row is not
  mistaken for preserving an old one. Pure removals and metadata-only updates
  do not revalidate untouched rows.
- A change to `spec.vmname` changes allocation ownership; revalidate all
  remaining rows under the new owner even if the row contents are unchanged.
- Keep MAC/IP and cross-object duplicate checks for the rows being validated.
  Untouched invalid foreign rows remain stored, not silently repaired,
  normalized or removed by another helper.
- Range-check an explicit `ipaddress` only while the IPPool serving its
  `networkname` exists. A VMNetCfg whose network has no pool yet is the
  intended ordering of a VM created before its pool, and the controller's
  ERROR-then-recover path remains its contract. This is the rule the
  static-ip annotation is deliberately stricter than.

Admission and helpers use the same network qualification rule: an unqualified
VM/VMNetCfg NIC network resolves in that object's namespace and is compared
with the fully qualified pool network. Equivalent qualified and unqualified
spellings cannot bypass the applicable range check. Comparison canonicalization
does not rewrite foreign stored rows.

Preserve the single-object ownership policy. `findRecordedTuple` explicitly
skips `other.Name == obj.Name`: mixed-network rows in one VMNetCfg do not
conflict with that object itself. The guard rejects a DISTINCT config in the
same namespace carrying the same `spec.vmname` and MAC, regardless of network.
Do not add network to that cross-object identity or weaken the guard merely
to support multiple networks. The controller-created object's name remains the
VM name, and there is still one VMNetCfg per VM.

The webhook remains global, with no helper network label, Multus attachment
or helper-local pool selector. Its IPPool lookup and deletion-reference index
must remain cluster-wide and unfiltered; duplicate lookup remains scoped to
the admitted object's namespace, not the infrastructure namespace. Webhook
Service selectors must never select helper pods.

Existing rules cover CREATE/UPDATE of the main VMNetCfg and IPPool resources
plus the static-ip annotation of VirtualMachine CREATE/UPDATE, not their
`/status` subresources. `UpdateStatus` is not currently admitted;
do not describe status acknowledgements as blocked by these webhook rules.
Keep that coverage and the existing failure policies: VMNetCfg and IPPool
spec entries use `Ignore`; the VirtualMachine static-ip rule uses the same
`Ignore` entry without a namespace selector; IPPool DELETE defaults to `Fail`. An explicit
validation denial is still a denial under `Ignore`. Controller ownership,
pool validation and unwind safeguards remain authoritative and unchanged.

### Serializing this helper's own work

The VM and VMNetCfg controllers share one reconciliation mutex per leadership
era. Both hold it from the fresh-state read through binding mutations, API
commits and immediate unwind. It is acquired before allocator locks; the
existing DHCP-before-IPAM lock order is retained, with no recursive acquisition
from an allocator callback.

This prevents a VMNetCfg reconciliation from restoring a NIC while the VM
controller is cleaning it but has not yet removed its spec row. API conflict
checks alone are insufficient when local state changes without a spec change.
All work resumed after waiting for the mutex must re-read current state.

One mutex deliberately serializes the two existing single-worker controllers
within a network. It does not serialize different network deployments or
block DHCP packet handling. Cross-network cooperation still uses the API
merge protocol above.

## 6. Cleanup and finalizer completion

Keep the existing cleanup finalizer:

```text
kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup
```

Do not add a finalizer for every network. Instead, a deleting VMNetCfg's
remaining spec and status rows become the durable record of outstanding
cleanup. Each helper removes only the rows it has finished cleaning.

### Deletion protocol

For an object with `deletionTimestamp` set:

1. Gather owned entries from spec and status, owned reservation records for
   this object, and any pending local unwind work. Do not create new
   allocations or re-add desired NIC entries.
2. For each owned binding, un-record its durable pool reservation, delete
   its owned DHCP lease and release its IPAM claim. Refresh the durable pool
   usage counts best-effort after local release, then update metrics. Complete
   pending unwinds before acknowledging cleanup. Already-absent state is
   idempotent success; another owner's state is retained.
3. After cleanup succeeds, fresh-read and remove the completed owned spec
   rows with a conflict-safe `Update`. Never remove an IP-bearing spec row
   before cleanup: it is the durable information needed to retry after a crash.
4. Fresh-read and remove the completed owned status rows with a conflict-safe
   `UpdateStatus`.
5. Fresh-read again. Remove the helper's cleanup finalizer only if **both
   arrays are empty** and this helper has no pending cleanup. Use a
   resourceVersion-checked metadata update. Preserve unrelated finalizers
   and retain the existing handling of the legacy helper cleanup marker.

A status row contains no IP address. Do not assume that a status-only row is
proof of completed cleanup: it can also predate this implementation. Resolve
any remaining binding from this network's owner-matching pool ledger and
local lease/claim state. Clean what remains; if the API cannot establish
whether a reservation exists, retain the row and retry. Never invent an IP
address or remove another owner's reservation.

Recovery uses owner-matching pool ledger records together with snapshots of
local DHCP leases and IPAM claims; a status row never supplies a guessed IP.
After local release, `ippoolstatus.UpdateAccounting` attempts to refresh durable
usage from the allocator. These counters remain best-effort: a failed refresh
does not gate row/finalizer acknowledgement or create accounting-only retries.
A same-owner ledger ADD remains a no-op, as before this feature.

Ledger owner strings identify namespace, VM name and canonical MAC, not the
VMNetCfg UID. Recovery cannot infer exclusive ownership from that string alone.
Before cleaning recovered tuples, fresh same-namespace spec/status references
preserve bindings still used by another config. Failed attribution lookup
retains cleanup state rather than declaring the record abandoned.

An empty deleting object still reaches the finalizer check. The normal
“no owned NICs” shortcut must not strand a completed deletion.

```text
Example: deleting a VMNetCfg with NICs A and B

                          spec rows   status rows   finalizer
Deletion starts           [A, B]      [A, B]        retained
A's resources cleaned     [A, B]      [A, B]        retained
A's spec acknowledged     [B]         [A, B]        retained
A's status acknowledged   [B]         [B]           retained
B finishes both writes    []          []            removable
Fresh-read + final Update []          []            removed
```

The VM controller must not re-add NICs to a deleting object. A stale live-path
status write must also recheck deletion state before committing.

For a NIC removed from a VM that is still running, use the same ordering:
clean that owned NIC first, then remove its spec/status entries. Preserve
all other networks' entries; do not delete the whole VMNetCfg just because
this helper no longer owns a NIC on it.

**Availability consequence:** all networks with outstanding entries need a
running helper to finish deletion. If storage's helper is down, management can
finish its own cleanup, but it cannot remove storage's rows or the finalizer.
A foreign spec-only or status-only row still blocks final completion. Drain
network-owned allocations before retiring that network's Deployment.

The IPPool deletion gate retains its cluster-wide lookup and includes the
canonical network in `buildAllocationOwnerIndex` and `evaluateIPPoolRecords`
matching. It reads VMNetCfg spec references across all namespaces, including
deleting objects, without any helper label selector. A matching NIC on another
network cannot keep an orphaned allocation in this pool blocking.
An unavailable index, unparseable owner reference or ambiguous network identity
remains blocking rather than being declared orphaned.

This gate does not wait on status rows or finalizers. Clean bindings before
removing their spec references as above. A webhook outage fails IPPool
deletion closed; keep the shared webhook available while helpers transition.

## 7. Implementation map

The runtime boundaries and their responsibilities are:

| File / area | Implemented responsibility |
| --- | --- |
| `pkg/util/network.go` | Immutable `NetworkScope`, qualified identity, the two-label pool selector, derived Lease name, pool matching, and pure owned-subset spec/status filters and merges. Bare references resolve in the containing object's namespace; zero scope owns nothing. |
| `pkg/util/staticip.go` | The static-ip annotation name and its decoder: a json object of interface name to canonical ipv4 address; absent or empty yields no request, a malformed value is an error for admission and a warning for the controller. |
| `pkg/controller/vmnetcfg/declared.go` | The declaration walk over the cluster's VirtualMachine list: one network's declared addresses keyed by canonical macaddress (the annotation on the VM's own metadata, the interface's Multus network resolved and qualified in the VM's namespace, non-Multus/empty/foreign entries and malformed annotations skipped), and the exclusion set a fresh allocation skips. |
| `pkg/app/app.go` | Own-pod identity validation before cleanup/election; scoped startup cleanup and era pool discovery; one reconciliation mutex per era shared by the VM and VMNetCfg controllers; unfiltered VMNetCfg startup LIST. |
| `cmd/kubevirt-ip-helper/main.go` | `Init` precedes startup cleanup; signal-driven service drain and Lease release remain in place. |
| `pkg/controller/ippool/event.go` | The same network selector on pool LIST and WATCH. |
| `pkg/controller/ippool/controller.go`, `ippool.go` | Identity validation before mutation and registration; actual deletion distinguished from selector loss; local teardown preserves a still-live pool's durable ledger. |
| `pkg/controller/vm/controller.go`, `event.go`, `vm.go` | Network-scoped projection and cleanup, era-local serialization, concurrent-create/spec merge handling, preservation of allocated addresses and foreign rows, the static-ip annotation read of its owned NICs, and a malformed-annotation warning that does not wedge the projection; unfiltered VM watcher. |
| `pkg/controller/vmnetcfg/controller.go`, `event.go`, `vmnetcfg.go` | Network-scoped startup/live/deletion/status/metrics handling, owner-checked allocation/unwind and startup settlement, the per-reconcile memoized static-ip declaration lookup and its declared-claim/excluding allocation branch; unfiltered VMNetCfg watcher. |
| `pkg/controller/vmnetcfg/merge.go` | Fresh-read, UID/owner/deletion/owned-state fenced spec and status commits with bounded context-aware conflict retries, preserving foreign entries and metadata. |
| `pkg/controller/vmnetcfg/cleanup.go` | Status-only and remaining-binding recovery, sibling-reference attribution before recovered binding cleanup, and shared finalizer acknowledgement. |
| `pkg/ippoolstatus/status.go` | Shared fresh-read, network-fenced and owner-checked ledger mutations; retries revalidate the canonical pool network. `UpdateAccounting` refreshes durable usage best-effort after local release without inventing a ledger mutation. |
| `pkg/ipam/ipam.go` | `IPsOwnedBy` snapshots owner-matching claims without adopting anonymous pins; `UsageCounts` snapshots accounting. `AllocateIPExcluding` skips a declared-address set for a fresh dynamic allocation while `AllocateIP` keeps its semantics (nil exclusion). Release still rechecks ownership. |
| Pool-existence checks in both VM controllers | Unfiltered, one-shot verification only after the owned-NIC guard. Cache misses and label loss are not proof of deletion and do not feed discovery. |
| `pkg/webhook/service/service.go` | CREATE/all-row versus UPDATE/`OldObject` delta validation, vmname-change revalidation and common network qualification; network-aware global pool-deletion references; unchanged distinct-object VM/MAC duplicate guard; VirtualMachine static-ip annotation validation against the durable pool status. |
| `cmd/kubevirt-ip-helper-webhook/main.go`, `pkg/webhook/admission/admission.go`, `pkg/webhook/config/` | Canonical singleton Service/TLS/CSR identity, existing admission resource/subresource coverage and failure policies, extended to VirtualMachine CREATE/UPDATE for the static-ip annotation; no network-specific webhook instance or selector. |
| Controller/app/webhook unit-test fixtures | Behavioral ownership, interleaved-write, startup, admission and cleanup-recovery cases use real network identities and the existing fake API harnesses rather than whole-object-write assumptions. |

Owner-checked allocation helpers, `verifyClaimedNics`, `unwindClaim` and the
VM controller's post-release ownership checks remain necessary: the local
mutex does not fence external writers. Conflict loops rebase API intent, not
allocation or release side effects. Small pure merge helpers provide the shared
boundary; the implementation does not introduce a controller framework.

Unchanged interfaces: DHCP packet behavior, CNI ownership of interfaces,
pool-per-interface validation, CRD schemas and generated clients. Both
`deployments/crds.yaml` and the chart's `crds/ippools.yaml` and
`crds/vmnetcfgs.yaml` define matching unchanged schemas without
`selectableFields`. Existing pods `get/update`
and Lease RBAC permissions suffice. Preserve the RoleBinding and
ClusterRoleBinding to the shared helper ServiceAccount for every Deployment,
including an operator-supplied account when chart account creation is disabled.
No VLAN field, CRD field or helper network environment variable is added.

## 8. Deployment migration and release qualification

### Deployment artifacts

Both deployment paths implement section 3. The chart root below is
`deployments/charts/kubevirt-ip-helper/`:

| File / area | Current behavior |
| --- | --- |
| `values.yaml` | `kubevirtiphelper.networks` supplies explicit names/interface/per-entry replicas; `serviceMonitor.enabled` controls the singleton monitor. Shared image/security/resources/scheduling and canonical webhook values remain. |
| `templates/kubevirtiphelper/_helpers.tpl` | Validates required unique per-network names without truncation collisions, including shared same-kind resources; enforces the installed namespace and webhook identity/ports; retains common labels and baseline shared fullname generation. |
| `templates/kubevirtiphelper/deployment.yaml` | One Deployment per network with disjoint selectors, exact network identity and a protected derived one-NAD annotation; plain-equivalent probes/lifecycle; explicit zero replicas remain zero. |
| `templates/kubevirtiphelper/service.yaml` | One explicitly named metrics Service per network with release/network labels and network/active-leader selectors. |
| `templates/kubevirtiphelper/servicemonitor.yaml` | At most one optional monitor selects all network metrics Services through common release labels. |
| `templates/kubevirtiphelper/{serviceaccount,roles,rolebindings,clusterroles,clusterrolebindings}.yaml` | Shared outside the network loop; RoleBinding and ClusterRoleBinding still target an operator-supplied account when account creation is disabled. |
| `templates/webhook/{_helpers.tpl,deployment.yaml,service.yaml,serviceaccount.yaml,roles.yaml,rolebindings.yaml,clusterroles.yaml,clusterrolebindings.yaml}` | Singleton canonical Service/namespace/ports and matching account/TLS/CSR/admission identity, including CA-bundle access in `kube-system`. |
| `deployments/deployment.yaml` | `helper-management`/`helper-storage` and `metrics-management`/`metrics-storage`, replicas 1, `app` plus network selectors, unchanged helper container name and shared helper account/RBAC. |
| `deployments/servicemonitor.yaml` | Separate optional shared plain-YAML monitor; the base helper manifest does not require the monitoring CRD. |
| `deployments/webhook-deployment.yaml` | One shared canonical webhook, never a copy per helper. |
| `README.md` | Both deployment methods, qualified tenant NAD references, pool labels, build entrypoints and explicit state-preserving Lease cutover/rollback. |
| `deployments/crds.yaml`, chart `crds/{ippools,vmnetcfgs}.yaml` | Matching unchanged schemas and existing installation path; no generated NADs, pools or per-network CRDs. |

Use new per-network Deployment names: an existing Deployment's selector is
immutable. Validate each helper's identity, attachment, interface and NAD CNI
configuration before enabling it. Add both identity labels to operator-managed
pools and fully qualify their network references without discarding status.

Use Helm OR plain YAML as the owner of an installation's shared resources,
not both. Adopting an existing plain-YAML installation into Helm is a separate
package-ownership migration; it must preserve the shared webhook and durable
state. Never delete/recreate CRDs to satisfy Helm ownership.

Preflight every pool against current admission validation before migration.
The IPPool-spec webhook revalidates the full IPv4 configuration even on
metadata-only label updates. Repair inadmissible pools safely before cutover
or in the same validated update; labels alone cannot bypass this gate.

A namespace move also moves the admission entries, whose names are
namespace-qualified. The webhook of the new namespace prunes the entries of the
previous one on startup, so stop the old webhook together with the old helper: a
webhook of the previous namespace re-adds its own entries on every start. Until
the prune lands, the apiserver calls a service which is gone, and the IPPool
deletion gate - the one entry with the default `failurePolicy: Fail` - rejects
every IPPool delete. The prune matches this helper's own service name, so entries
of another product in the same ValidatingWebhookConfiguration stay untouched.

The normal cutover below assumes the served NAD identities already reside in
`kubevirt-ip-helper`. The legacy chart default referenced a NAD in
`kubevirt-public`; moving that NAD into the shared namespace
changes network identity, not just labels. Drain that network's allocations
under its old helper, recreate its NAD and pool centrally, and update the
tenant VM references before retiring the old helper. Do not relabel a live
pool or rewrite live reservation references across namespaces. Keep new
helpers stopped until the stop-old/wait-for-pods boundary has completed.

Do not install a chart release per network. One release owns the shared
webhook in the fixed namespace. Duplicated webhook runtimes would otherwise
target the same hardcoded admission registration, TLS Secret and CSR even if
their static resource names differed.

### Cutover order

The old helper and the new per-network helpers use different Lease names.
Those locks do not exclude one another. A rolling image replacement across
that boundary could run two DHCP authorities for the same network.
Helm upgrade ordering is not exclusion between these Lease domains. Preserve
the one shared webhook Deployment and Service throughout helper replacement;
scaling helpers down must not scale admission down.

Use a stop-old, start-new cutover:

1. Save the current manifests and reservation state, complete pool preflight,
   and deploy the admission delta/canonicalization changes. Prepare the new
   helper Deployments at zero replicas and verify the network-to-pool mapping.
2. Scale the old helper Deployment to zero and wait until all its pods have
   terminated. Do not enable new leaders while an old helper can still serve.
3. Apply the pool labels and new Service configuration. Preserve IPPool
   reservation status and VMNetCfg state for startup replay.
4. Start the per-network Deployments. Verify each network has one leader,
   its expected registered pool and a working DHCP exchange from a guest.
5. Verify metrics endpoints select only their respective network leaders.
   Remove the obsolete Deployment, Service and Lease after successful cutover.

This introduces a DHCP service interruption during handover rather than an
unsafe overlap. The obsolete Lease object is not automatically deleted when
its holder stops renewing it.

Rollback uses the same exclusion rule: stop all new helpers before restarting
the old deployment with its original attachments. Preserve the latest durable
reservation state; do not overwrite new allocations with a stale backup.

### Build entrypoints and image consumers

The helper image is built from `build/Dockerfile`, with repository root as
build context; its CLI build target is `./cmd/kubevirt-ip-helper`.
The webhook image uses `build/Dockerfile.webhook`, also with repository root
as context, and target `./cmd/kubevirt-ip-helper-webhook`.
`test/e2e/run.sh` builds and loads both images using their explicit Dockerfiles:

```sh
"${RUNTIME}" build -f "${ROOT_DIR}/build/Dockerfile" -t "${E2E_IMAGE}" "${ROOT_DIR}"
"${RUNTIME}" build -f "${ROOT_DIR}/build/Dockerfile.webhook" -t "${E2E_WEBHOOK_IMAGE}" "${ROOT_DIR}"
```

Keep `ROOT_DIR` as build context; changing context to `build/` would omit the
Go module and sources.

### E2E harness and qualification boundary

The adapted harness uses two independent helper deployments rather than two
NADs on one helper:

- `test/e2e/versions.env` defines primary `helper-e2e`, `metrics-e2e` and
  `kubevirt-ip-helper-lock-kubevirt-ip-helper-e2e`, plus secondary
  `helper-e2e2`, `metrics-e2e2` and
  `kubevirt-ip-helper-lock-kubevirt-ip-helper-e2e2`. Both use the shared
  infrastructure namespace and network-qualified helper selectors.
- `test/e2e/manifests/` and its `secondary/` overlay supply the identity labels,
  one-NAD attachments and matching pools. Primary-only lanes install only the
  primary helper pair; the multipool lane adds the secondary pair.
- `test/e2e/run.sh` builds/loads/deploys the standalone canonical webhook and
  includes Service/endpoint, VWC, TLS/CSR/certificate and admission-denial
  qualification instead of disabling admission for invalid fixtures.
- The multipool scenario exercises simultaneous A/B guest DHCP service and
  primary-helper failover. It checks that B retains its independent Lease,
  leader-selected metrics target and working guest service while A changes
  leadership.
- `test/e2e/collect.sh` and `evidence.sh` use per-network helper, Lease, Service
  and EndpointSlice evidence alongside the one webhook/VWC; a missing secondary
  network in a primary-only lane is not an evidence failure.

E2E and live-cluster qualification have **not** been run for this implementation.
The adapted scenarios specify the required real two-network DHCP, admission
and failover proof; they are not a substitute for executing it.

## 9. Acceptance criteria

The implementation and release qualification must defend these observable
contracts. The matrix states required outcomes, not a blanket claim that
unit/race suites or live-cluster qualification have passed.

| Scenario | Required result |
| --- | --- |
| Missing/invalid pod identity or own-Pod read failure | Startup fails before leader election or network mutation. |
| Same NAD name appears outside the infrastructure namespace | Its pool is not discovered by the shared-namespace helper, including after startup; namespace-qualified identity prevents accidental adoption. |
| VMs in tenant-a and tenant-b use management | Both reference the single `kubevirt-ip-helper/management` NAD; no per-tenant NAD or helper copies are required. |
| Two-network chart values | Exactly two helper Deployments and metrics Services; each has its own validated names, exact network label, one-NAD annotation and disjoint matching selectors. Explicit zero replicas remain zero; omitted replicas default to one. |
| Shared chart resources and account bindings | One helper account/RBAC set and one webhook Deployment/Service/account/RBAC set; every helper uses the shared account with pod/Lease permissions. No network-generated NADs, IPPools or CRD copies. |
| Chart probes and lifecycle | Every helper has the plain manifest's startup/readiness/liveness behavior, rollout settings and termination grace; healthy standbys remain ready without serving DHCP. |
| Metrics and optional monitoring | The common-release ServiceMonitor discovers both network metrics Services, each routing only to its own leader; disabling the monitor permits installation without its CRD. Plain YAML keeps its own consistent label convention. |
| Invalid or colliding per-network names | Rendering rejects missing, invalid or duplicate names rather than truncating them into collisions with another network or shared resource. |
| Singleton admission routing and TLS | The one VWC routes to `kubevirt-ip-helper-webhook.kubevirt-ip-helper.svc:8080`, Service forwards to listener 8443, and Secret/CSR/certificate identities agree; webhook endpoints never include helper pods. |
| Admission UPDATE with unchanged invalid B rows | A's valid allocation/spec update, removal and metadata acknowledgement succeed while B's rows remain unchanged; newly added or modified invalid rows are rejected. |
| Admission CREATE or changed spec.vmname | All new rows, or all remaining rows under the changed owner, are revalidated; ownership changes cannot bypass duplicate/MAC/IP guards. |
| Mixed-network object versus a duplicate object | Rows inside one shared VMNetCfg do not conflict with themselves; a distinct object with the same namespace/vmname/MAC is still rejected even on another network. |
| Qualified/unqualified network references | Admission and helper comparisons resolve the same NAD identity; equivalent spelling does not bypass IP range validation or mutate foreign stored rows. |
| VM static-ip annotation with an address the pool can serve | Admission admits the VM, the owned VMNetCfg row carries that address and the durable pool status records it for that VM; the guest is served it. |
| VM static-ip annotation the durable pool cannot serve | Admission rejects the VM: malformed value, interface name unknown to that VM, missing/non-Multus/empty networkName, no IPPool for that NIC's network, out of range, exclude or broadcast entry, another VM's recorded address, or the same address twice inside one VM. |
| Two VMs admitted for the same free address | Both are admitted, because admission reads the durable ledger only; the loser's interface records a sticky ERROR status at reconcile until the address is freed. |
| A dynamic VM and a declared address | The fresh dynamic allocation skips every address the cluster's VirtualMachines declare for that network, so a NIC which asks for nothing takes another free one; the declaring NIC claims exactly its declared address, and a declared address which another owner holds is refused with the same ERROR status and retry as any other refused claim. |
| Static-ip annotation added to a running VM | The reconcile claims the declared address and the durable pool status records it for that VM, while the guest keeps the address it was served until its next DHCP request, up to its lease time, and is answered with the declared one from then on. |
| Static-ip annotation removed, or malformed at reconcile | Removal releases the address and returns that interface to a dynamic address; a malformed value is warned about and ignored without wedging the VM's other interfaces or the projection of its rows. A helper restart between the removal and its reconcile keeps the stored address served, with no release, until the next annotation change or NIC removal. |
| Admission status coverage and pool deletion | Status writes remain outside current webhook rules. Global spec-reference lookup matches namespace/VM/network/MAC: a matching NIC on the same network blocks deletion, while a NIC only on another network cannot keep an orphaned record blocking. Ambiguous references, lookup failure and webhook outage remain fail-closed. |
| Pool-label migration with an invalid existing spec | Preflight identifies the invalid pool; metadata-only relabelling is not claimed to bypass spec validation. |
| Selected pool has a mismatched network or missing interface | No successful registration; the error remains visible and startup is not falsely completed for that pool. |
| Two replicas for A; another Deployment for B | A has one active leader, B has its own; losing A's leader does not change B's Lease or metrics target. |
| Foreign-only VM/VMNetCfg with no local work | No object/resource mutations; the VMNetCfg startup key settles as a no-op. |
| Mixed-network VM creation and reconciliation | Both networks' spec/status entries survive interleaved creates and updates; each helper changes only its own entries and metrics. |
| Conflicting API write or concurrent allocated-IP update | Re-read and merge preserve newer entries and IP assignments without duplicating reservations or leases. |
| Object replaced, deleted or NIC changed during an allocation commit | No stale write; newly applied work is owner-safely unwound or completed by deletion. |
| Allocation commit succeeds but its API response is lost | Fresh verification preserves the owner-matching binding for the baseline or exact intended allocated-IP row, never a different changed owned IP. |
| Cache miss or pool label loss while the pool still exists | No false deletion verdict and no abandoned durable reservation. Verification results do not enter discovery. |
| A NIC is removed from a live multi-network VM | Only that network's resources and entries are removed; the other network remains operational. |
| Live NIC removal overlaps local VMNetCfg restoration | Shared local serialization prevents the removed NIC's lease or reservation from being recreated during cleanup. |
| One helper completes shared-object deletion first | Its resources and rows are removed, but foreign spec-only and status-only rows keep the finalizer. |
| Crash between cleanup, spec acknowledgement and status acknowledgement | Replay completes idempotently; the last helper removes the finalizer only after both arrays are empty. |
| Status-only row inherited from older state | Any owner-matching reservation is cleaned before acknowledgement; no address is guessed and foreign reservations are preserved. |
| Cleanup releases a local claim | Durable usage is refreshed best-effort after local release; counter convergence is not a row/finalizer completion condition. |
| Recovered binding also has a sibling config reference | Fresh same-namespace spec/status references preserve the sibling's binding before cleanup effects; failed attribution lookup retains cleanup. |
| Deleting object already has empty arrays | The finalizer check still runs; an early no-work return cannot strand the object. |

No implementation is complete merely because it passes single-network tests.
The shared-object cases must exercise two network identities against the same
API-backed VMNetCfg state. Deployment qualification additionally exercises the
real two-network DHCP and failover paths described above.
