# Operations

## Install

Create the runtime token first:

```sh
kubectl create namespace operator-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n operator-system create secret generic smolvm-runtime-auth \
  --from-literal=token="$(openssl rand -base64 48)"
```

Build and apply a versioned artifact:

```sh
make rc-artifacts VERSION=<version> IMG=<operator-image> RUNTIME_IMG=<runtime-image>
kubectl apply -f dist/smolvm-operator-<version>.yaml
```

## Runtime requirements

Each runtime node needs `/dev/kvm`, `/var/lib/smolvm`, `/var/run/smolvm`, and network connectivity from the controller to runtime pods on TCP 9443.

## Upgrade

1. Apply the generated RC artifact.
2. Wait for controller Deployment rollout.
3. Wait for runtime DaemonSet rollout.
4. Confirm existing VMs retain `status.nodeName` and `status.machineName`.
5. Confirm no duplicate runtime machines are created.
6. Exercise stop, start, and delete on an upgraded VM.

Rollback uses the previous install artifact. Do not roll back CRDs after creating objects with fields unsupported by the older controller.

## Deletion and force delete

Normal deletion keeps the finalizer until the owning runtime deletes local state or reports the machine missing.

If the owning runtime is unavailable, deletion is blocked with `DeletionBlocked=True`. To remove only the Kubernetes object while accepting possible orphaned local state:

```yaml
metadata:
  annotations:
    vm.smolvm.dev/force-delete-local-state: "true"
```

Record `status.nodeName` before force delete and clean up local state manually on that node.

## Observability

Status conditions:

- `SmolVM`: `Scheduled`, `Ready`, `RuntimeReady`, `GuestReady`, `Reconciled`, `DeletionBlocked`.
- `SmolVMNode`: `Ready`, `KVMAvailable`, `RuntimeReady`, `Schedulable`.

Expected events/logs include create, start, stop, resize, delete, invalid spec, unsupported update, runtime unavailable, deletion blocked, and force delete paths.

Recommended alert inputs:

- stale `SmolVMNode.status.heartbeatTime`;
- `SmolVM` with `DeletionBlocked=True` beyond SLO;
- controller Deployment unavailable;
- runtime DaemonSet unavailable;
- repeated `RuntimeUnavailable` warning events.
