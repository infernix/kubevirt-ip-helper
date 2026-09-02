# Test Coverage TODO

The test-only branch establishes passing coverage for the current behavior without changing production code. The remaining work requires production fixes, explicit dependency seams, or privileged integration infrastructure.

## Current baseline

| Package | Coverage |
|---|---:|
| `pkg/cache` | 100.0% |
| `pkg/ipam` | 100.0% |
| `pkg/util` | 100.0% |
| `pkg/metrics` | 95.6% |
| `pkg/controller/vm` | 94.2% |
| `pkg/dhcp` | 91.0% |
| `pkg/controller/vmnetcfg` | 82.4% |
| `pkg/controller/ippool` | 63.4% |
| `pkg/app` | 45.8% |
| `pkg/apis/kubevirtiphelper.k8s.binbash.org/v1` | 46.4% |
| `pkg/network` | 42.9% |

The combined profile for the explicitly selected authored packages is 74.3%. `go test -cover ./...` also includes generated clientsets, informers, listers, apply configurations, and deepcopy code; most generated packages remain at 0%.

## Production defects blocking green correctness tests

### P0: lifecycle and deletion safety

- [ ] Make `pkg/dhcp.DHCPAllocator.Stop` safe when the NIC has no registered server or the stored server is nil. It currently calls `a.servers[nic].Close()` without a guard and can panic.
- [ ] Unwrap `cache.DeletedFinalStateUnknown` before reading IPPool fields in the `pkg/controller/ippool` delete callback.
- [ ] Unwrap `cache.DeletedFinalStateUnknown` before reading VirtualMachine fields in the `pkg/controller/vm` delete callback.
- [ ] Add callback-level ADD, UPDATE, DELETE, and tombstone tests after delete extraction is safe and directly testable.

### P0: reconciliation correctness

- [ ] Return reconciliation failures from `sync` in the IPPool, VM, and VMNetCfg controllers so `processNextItem` can use the existing rate-limited retry policy. Current failures are logged and then forgotten.
- [ ] Handle `DHCPAllocator.AddLease` failures in `pkg/controller/vmnetcfg`. Do not report an allocation as successful when no DHCP lease was created.
- [ ] Define rollback or transaction ordering for IPAM, DHCP, IPPool status, VMNetCfg spec/status, cache, and metrics mutations. Later API failures currently leave earlier side effects committed.
- [ ] Validate a replacement IPPool before deleting the active DHCP pool. `createOrUpdateDHCPPool` currently removes the old pool before parsing the replacement subnet.
- [ ] Decide whether `BindInterface` changes require restart; it is currently omitted from IPPool restart classification.

### P1: concurrency

- [ ] Protect `pkg/cache.CacheAllocator` maps across concurrent controller access.
- [ ] Audit and synchronize DHCP pool, lease, and server reads as well as writes; DHCP handlers run concurrently with controller reconciliation.
- [ ] Audit IPAM existence checks, create/delete, count, and allocation operations under one consistent locking policy.
- [ ] Synchronize app status and startup counters shared between the app loop and controller workers.
- [ ] Add focused concurrent transition tests and run them under `go test -race` after synchronization is fixed.

### P1: application lifecycle

- [ ] Make the leader callback honor its supplied context and return without relying on `os.Exit(1)`.
- [ ] Replace fixed polling sleeps with an injectable clock or event-driven synchronization.
- [ ] Ensure one invalid initial IPPool cannot leave `RunServices` waiting forever for the successful-registration count.
- [ ] Preserve the default namespace when reading the namespace file fails; `Init` currently assigns the fallback and then overwrites it with the empty read result.
- [ ] Bound metrics HTTP shutdown with a timeout rather than `context.Background()`.

### P2: utility error handling

- [ ] Make `pkg/util.FileExists` return false or an error for every failed `os.Stat` call before dereferencing `info`. Some non-NotExist failures can leave `info` nil.
- [ ] Replace Kubernetes error-string matching with `apierrors.IsNotFound` and `apierrors.IsConflict` where applicable.
- [ ] Replace hard-coded conflict retry sleeps with an injectable backoff/clock so exhaustion can be tested quickly and deterministically.

## Remaining test work after production seams exist

### IPPool

- [ ] Successful `registerIPPool` including NIC address, DHCP listener, IPAM exclusions, status, metrics, and cache reconstruction.
- [ ] Rollback at every failure boundary after NIC mutation.
- [ ] Restart-required changes and full cleanup ordering.
- [ ] Idempotent cleanup when individual DHCP, IPAM, cache, metrics, or netlink state is already absent.
- [ ] Informer delete tombstones and cancellation during active watches.

### VMNetCfg

- [ ] Rollback tests for failures after IP allocation, DHCP lease creation, IPPool status update, spec update, and status update.
- [ ] Cleanup continuation and error aggregation for missing lease, missing subnet, missing pool, and API failures.
- [ ] Conflict retry exhaustion using an injected backoff.
- [ ] Concurrent VM allocations against one pool after allocator synchronization is fixed.

### VM

- [ ] Informer delete tombstones.
- [ ] Conflict retry exhaustion using an injected backoff.
- [ ] Two-controller VM-to-VMNetCfg handoff under API conflicts and deletion races.

### App and process lifecycle

- [ ] Leader acquisition, loss, cancellation, and reacquisition without process termination.
- [ ] `RunServices` startup ordering and failure propagation.
- [ ] Application restart teardown and reconstruction.
- [ ] Signal handling in a subprocess without hard-wired exit behavior.
- [ ] Metrics and DHCP service readiness and graceful shutdown.

### Network and DHCP integration

- [ ] Netlink add/remove success and idempotency in a disposable privileged network namespace.
- [ ] DHCP `Run`/`Stop` with an injected listener or isolated namespace instead of binding host UDP/67.
- [ ] Packet behavior against an actual UDP listener, including shutdown during traffic.

## Coverage scope decision

Before enforcing a 100% threshold, decide which code is in scope:

- [ ] Authored production packages only; exclude `pkg/generated/**` and generated deepcopy output.
- [ ] Full module including generated code; this requires generator-boilerplate tests with limited defect-detection value.
- [ ] Separate unit and privileged-integration thresholds so host lifecycle paths do not make ordinary `go test` depend on root privileges.

Recommended target: 100% for pure allocators and utilities, high branch-focused coverage for controllers, and explicit privileged integration coverage for netlink, DHCP service lifecycle, leader election, and process shutdown.

## Verification gate

```text
gofmt -d <changed Go files>
go test -mod=vendor -count=1 ./...
go test -mod=vendor -count=1 -race ./...
go test -mod=vendor -count=1 -cover ./...
go build -mod=vendor -o /tmp/kubevirt-ip-helper .
```
