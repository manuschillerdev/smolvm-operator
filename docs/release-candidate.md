# Release candidate readiness

A build is RC-ready only when every required gate below is complete.

| Area | Gate | Evidence |
| --- | --- | --- |
| Install artifact | Versioned install manifest is reproducible and has a checksum. | `make rc-artifacts VERSION=<version> IMG=<operator-image> RUNTIME_IMG=<runtime-image>` |
| Controller tests | Controller envtest passes with coverage and JUnit output. | `make test-report` |
| Topology e2e | KVM-backed kind deployment e2e passes. | `make test-e2e-report` in CI |
| Runtime lifecycle e2e | Create, run, stop, and delete cleanup pass against real smolvm runtime. | `make test-runtime-e2e-report` in CI |
| Upgrade e2e | Previous install artifact upgrades to the RC without losing VM identity. | `make test-upgrade-e2e-report` in CI with `PREVIOUS_INSTALL_MANIFEST_URL` |
| Failure recovery | Runtime unavailability, stale heartbeat, blocked deletion, and force-delete paths are tested. | `make test-failure-e2e-report` in CI |
| Multi-node behavior | Scheduling, pinning, selectors, port conflicts, and no silent migration are tested on at least two runtime nodes. | `make test-multinode-e2e-report` in CI |
| Security | RBAC, privileged runtime, token handling, NetworkPolicy, image provenance, and hostPath use are reviewed. | `docs/security-review.md` |
| Observability | Health checks, events, conditions, logs, and alert inputs are documented. | `docs/operations.md` |
| Compatibility | Supported Kubernetes, smolvm, CPU architecture, container runtime, and KVM requirements are published. | `docs/compatibility.md` |

## CI requirements

- E2E jobs run on linux/amd64 runners with `/dev/kvm`.
- `PREVIOUS_INSTALL_MANIFEST_URL` must point to an immutable previous release install manifest.
- Main-branch CI publishes immutable operator and runtime images tagged by commit SHA.

## Known limitations

- Bound VMs remain on their recorded node and require operator action after node-local data loss.
- Signed images, SBOMs, and manifest signatures are recommended follow-up release hardening items.
