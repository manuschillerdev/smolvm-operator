# smolvm operator

Kubernetes operator for declaratively managing node-local smolvm machines.

The default install deploys a cluster-level `smolvm-controller` Deployment and a
node-local `smolvm-runtime` DaemonSet. Users do not run `smolvm serve` manually:
the runtime DaemonSet owns KVM access, local state, the smolvm socket, runtime
health, and `SmolVMNode` capability reporting.

## Architecture

```text
smolvm-controller Deployment
  - schedules and reconciles SmolVMs
  - writes SmolVM status and finalizers
  - resolves node runtime endpoints from SmolVMNode
  - calls the selected runtime over an authenticated node API

smolvm-runtime DaemonSet
  - runs on eligible nodes
  - supervises smolvm serve
  - owns /dev/kvm, /var/lib/smolvm, and /var/run/smolvm
  - exposes the per-node runtime API
  - reports SmolVMNode status
```

`SmolVMNode` is cluster-scoped. Object names match Kubernetes node names, and
`status.nodeUID` lets the controller detect stale endpoints after node
replacement.

## Runtime API

The controller calls the runtime endpoint advertised in `SmolVMNode.status`.
The default manifests use bearer-token authentication through the
`smolvm-runtime-auth` Secret. Production installs should replace the default
Secret value and may add certificate management and NetworkPolicy.

Runtime operations are node-local and idempotent:

- `GetMachine`
- `CreateMachine`
- `EnsureMachineRunning`
- `StopMachine`
- `DeleteMachine`
- `ResizeMachine`
- `Health`
- `Identity`

## Scheduling

`spec.nodeName` is optional. If omitted, the controller selects an eligible
`SmolVMNode` and records the immutable assignment in `status.nodeName`.

Rules:

- `spec.nodeName` is an optional hard pin.
- `status.nodeName` is the actual binding.
- bound VMs do not move automatically.
- node/runtime loss updates status; it does not trigger silent migration.

## Deletion

Finalizers are node-aware. On delete, the controller calls the runtime endpoint
for `status.nodeName` and removes the finalizer only after local deletion
succeeds or the runtime reports the VM missing.

If the owning runtime is unavailable, deletion is blocked with a condition. The
escape hatch below removes the finalizer while accepting possible orphaned local
state:

```yaml
metadata:
  annotations:
    vm.smolvm.dev/force-delete-local-state: "true"
```

When the runtime is available, the controller always attempts normal cleanup.

## Example

```yaml
apiVersion: vm.smolvm.dev/v1alpha1
kind: SmolVM
metadata:
  name: alpine
spec:
  running: true
  image: alpine:latest
  nodeSelector:
    kubernetes.io/arch: amd64
  resources:
    cpus: 1
    memoryMiB: 512
  storage:
    storageGiB: 20
    overlayGiB: 10
  network:
    enabled: true
    ports:
      - hostPort: 8080
        guestPort: 80
```

## Development

```sh
make generate manifests fmt vet build
```

Install CRDs:

```sh
make install
```

Build and deploy the full operator stack:

```sh
make docker-build IMG=<registry>/smolvm-operator:<tag>
make docker-build-runtime RUNTIME_IMG=<registry>/smolvm-runtime:<tag>
make deploy IMG=<registry>/smolvm-operator:<tag> RUNTIME_IMG=<registry>/smolvm-runtime:<tag>
```
