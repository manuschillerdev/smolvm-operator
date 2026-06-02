# E2E test feasibility

| # | Test | Feasibility against smolvm | Operator readiness | Notes |
| --- | --- | --- | --- | --- |
| 1 | CRD install / API discovery | Feasible | Ready | Kubernetes-only test. |
| 2 | Manager deployment health | Feasible | Ready as DaemonSet | Use DaemonSet rollout status, not Deployment availability. |
| 3 | Invalid spec handling | Feasible | Ready | Controller marks invalid specs as `Failed`; admission rejection is not implemented. |
| 4 | Runtime unavailable behavior | Feasible | Ready | Kubernetes-only negative test with no `smolvm serve`. |
| 5 | Create + start lifecycle | Feasible | Ready | smolvm supports create and start; CI must provide `/dev/kvm` and libkrun runtime libraries. |
| 6 | Create stopped lifecycle | Feasible | Ready | smolvm create returns `created`; controller reports non-running as not Ready but reconciled. |
| 7 | Start after patch | Feasible | Ready | Patch `spec.running` from `false` to `true`; controller starts the existing machine. |
| 8 | Stop after patch | Feasible | Ready | smolvm supports stop; status should become `Stopped`. |
| 9 | Delete finalizer cleanup | Feasible | Ready | smolvm supports delete and stops the VM first if needed. |
| 10 | Delete when runtime machine already gone | Feasible | Ready | smolvm returns 404; controller already ignores not-found on delete. |
| 11 | Storage shrink rejection | Feasible | Ready | Both smolvm and controller reject shrink; controller should report `InvalidStorageResize`. |
| 12 | Storage expansion while running | Feasible | Ready | smolvm rejects resize while running; controller prevents the call and reports stop-required. |
| 13 | Storage expansion while stopped | Feasible | Mostly ready | smolvm supports stopped resize; test should use small sizes because disk expansion costs CI time and space. |
| 14 | Node filtering | Feasible | Ready | smolvm has no node field; this is operator-side filtering via `SMOLVM_NODE_NAME`. |
| 15 | Node-local missing assignment | Feasible | Ready | Operator-side behavior when `SMOLVM_NODE_NAME` is set and `spec.nodeName` is empty. |
| 16 | Status observedGeneration | Feasible | Ready | Kubernetes-only assertion after create/patch reconciliation. |
| 17 | Reconciliation idempotency | Feasible | Ready | smolvm create is not used repeatedly once machine exists; assert one stable machine name. |
| 18 | Controller restart recovery | Feasible | Ready | smolvm persists machine records and API can observe existing machines after controller restart. |
| 19 | Spec drift behavior | Partially feasible | Not mature | smolvm create-time fields such as image, CPU, memory, network, ports are not updated by current operator. Mature behavior needs immutability/admission or explicit drift status before this can be a passing test. |
| 20 | Host port / networking contract | Partially feasible | Not mature | smolvm supports `network`, `ports`, and `allowedCidrs` at create/start. The operator maps these fields but does not reserve host ports or detect cluster-wide conflicts. Test positive mapping first; conflict tests need new validation/status behavior. |

## Recommended mature e2e suite

### Always-on CI

- CRD install / discovery.
- DaemonSet rollout.
- Invalid spec status.
- Runtime-unavailable status.
- Node filtering and missing assignment.
- Idempotency using a fake or unavailable runtime.

### KVM runtime CI

- Create running machine.
- Create stopped machine.
- Patch start.
- Patch stop.
- Delete finalizer cleanup.
- Delete after out-of-band runtime deletion.
- Storage shrink rejection.
- Resize requires stopped machine.
- Stopped storage expansion.
- Controller restart recovery.

### Requires operator changes first

- Spec drift must be defined as either immutable, mutable, or explicitly unreconciled.
- Host port conflicts must be validated or surfaced deterministically.
