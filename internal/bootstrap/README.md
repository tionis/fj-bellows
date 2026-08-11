# internal/bootstrap

Renders the **provider-agnostic** cloud-init that prepares a worker VM. The
template (`cloud-init.yaml.tmpl`) is embedded at build time with `//go:embed`.

The bootstrap deliberately holds **no Forgejo credentials**: it ensures Docker
and the `forgejo-runner` binary are available, then touches a readiness sentinel
(`DefaultReadyFile`). The orchestrator registers an ephemeral runner per job and
delivers the one-shot token over SSH at dispatch time.

The install step detects the VM architecture (`uname -m`) and fetches the
matching `forgejo-runner` build, so both amd64 and arm64 workers are supported.
Set `PreparedImage` when those invariant packages and the pinned runner were
baked into the provider image. Cloud-init then performs only worker-specific
identity, SSH, swap, and readiness work, reducing cold-start latency and avoiding
the per-VM package-manager and download cost.
The orchestrator's public SSH key is also rendered into cloud-init, allowing
providers without a native authorized-key API to use the same bootstrap.

```go
userData, err := bootstrap.Render(bootstrap.Params{RunnerVersion: "12.10.1"})
```
