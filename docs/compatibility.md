# Compatibility matrix

| Component | Supported for RC | Notes |
| --- | --- | --- |
| Kubernetes | 1.29 | Envtest and kind validation use Kubernetes 1.29 assets/configuration. |
| smolvm | 0.8.1 | Runtime image downloads the pinned upstream smolvm release. |
| Runtime CPU architecture | linux/amd64 | smolvm 0.8.1 publishes `linux-x86_64`; runtime e2e must run on amd64. |
| Operator CPU architecture | linux/amd64, linux/arm64 | Go binaries are buildable for both; runtime image support is constrained by smolvm artifacts. |
| Container runtime | containerd via kind/Kubernetes CRI | Other CRI implementations require separate validation. |
| Host virtualization | KVM at `/dev/kvm` | Required on every schedulable runtime node. |
| Runtime state | `/var/lib/smolvm`, `/var/run/smolvm` host paths | Mounted by the runtime DaemonSet. |
| Runtime API | HTTPS on pod port 9443 | Bearer token from `smolvm-runtime-auth`; ingress restricted by NetworkPolicy. |

## Known limitations

- Bound VMs are node-local and do not migrate automatically after node or runtime loss.
- Force delete removes the Kubernetes finalizer while accepting possible orphaned node-local state.
