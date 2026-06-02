# smolvm operator

Kubernetes controller for declaratively managing node-local smolvm machines.

The operator reconciles `SmolVM` custom resources into calls to a node-local
`smolvm serve` API. It intentionally keeps Kubernetes reconciliation separate
from the privileged VM runtime: the controller owns desired state, while the
smolvm daemon owns KVM, libkrun, local disks, sockets, and machine processes.

## Runtime contract

Each node that runs `SmolVM` resources needs:

- a running `smolvm serve` API
- KVM access (`/dev/kvm` on Linux)
- libkrun/libkrunfw and smolvm runtime assets
- a persistent host-local smolvm state directory
- protected API access, preferably through a Unix socket

The controller reads these environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `SMOLVM_API_URL` | `http://127.0.0.1:8080` | smolvm API base URL |
| `SMOLVM_API_SOCKET` | empty | Unix socket path; overrides network dialing when set |
| `SMOLVM_NODE_NAME` | empty | node identity for node-local reconciliation |

## Current MVP behavior

The `SmolVM` reconciler supports:

- stable smolvm machine names per CR
- finalizer-based delete cleanup
- create/start/stop/delete lifecycle
- image or `.smolmachine` source selection
- CPU/memory creation settings
- storage/overlay creation settings
- expand-only storage invariant checks
- outbound networking and host port mappings
- status phase and `Ready`/`Reconciled` conditions
- runtime-unavailable backoff

This is an MVP controller, not a full pod runtime. Networking uses smolvm's
current API model; it does not provide Kubernetes CNI/Pod-IP semantics.

## Example

```yaml
apiVersion: vm.smolvm.dev/v1alpha1
kind: SmolVM
metadata:
  name: alpine
spec:
  running: true
  image: alpine:latest
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

Run locally against a smolvm API:

```sh
SMOLVM_API_URL=http://127.0.0.1:8080 make run
```

Or over a Unix socket:

```sh
SMOLVM_API_SOCKET=/var/run/smolvm/smolvm.sock make run
```

The default Kubernetes DaemonSet manifest mounts `/var/run/smolvm` from each
node and talks to `/var/run/smolvm/smolvm.sock`.
