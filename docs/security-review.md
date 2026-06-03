# Security review

## RBAC

The controller/runtime service account requires:

| Resource | Verbs | Reason |
| --- | --- | --- |
| `smolvms` | `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` | Reconcile VM lifecycle and finalizers. |
| `smolvms/status` | `get`, `update`, `patch` | Publish scheduling, runtime, and deletion state. |
| `smolvms/finalizers` | `update` | Hold deletion until node-local cleanup completes. |
| `smolvmnodes` | `get`, `list`, `watch`, `create`, `update`, `patch` | Runtime endpoint discovery and runtime node reporting. |
| `smolvmnodes/status` | `get`, `update`, `patch` | Runtime heartbeat, endpoint, and capability reporting. |
| `nodes` | `get`, `list`, `watch` | Scheduling, selectors, and node readiness checks. |
| `events` | `create`, `patch` | Lifecycle and failure event reporting. |

## Runtime privileges

The runtime DaemonSet is privileged and mounts `/dev/kvm`, `/var/lib/smolvm`, and `/var/run/smolvm`. Schedule it only on trusted KVM-capable worker nodes.

## Runtime API token

The manifests require a `smolvm-runtime-auth` Secret in the operator namespace; no reusable default token is shipped.

```sh
kubectl create namespace operator-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n operator-system create secret generic smolvm-runtime-auth \
  --from-literal=token="$(openssl rand -base64 48)"
```

Rotate by replacing the Secret and restarting the controller Deployment and runtime DaemonSet.

## NetworkPolicy

`config/runtime/networkpolicy.yaml` restricts runtime API ingress on TCP 9443 to pods labeled `control-plane=controller-manager` in the operator namespace.

## Image provenance

RC artifacts should use immutable operator and runtime image tags or digests. Main-branch CI publishes SHA-tagged images to GHCR.
