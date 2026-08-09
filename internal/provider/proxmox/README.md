# internal/provider/proxmox

The Proxmox provider creates QEMU workers by cloning a cloud-init
template through the Proxmox VE REST API. It uploads unique user data to a
snippet-capable storage, enables the QEMU guest agent, starts the clone, and
returns the first globally scoped address reported by the guest agent.

Required `provider_config` fields are `endpoint`, `token_id`, `token_secret`,
`node`, `template_vmid`, `snippet_storage`, and `network`. The endpoint must use
HTTPS. `ca_file` can add a private Proxmox CA to the system trust roots; TLS
verification cannot be disabled.

The source template must:

- be a QEMU VM template with a cloud-init drive;
- boot with the configured `ci_user` (default `root`);
- contain and enable `qemu-guest-agent`; and
- reside on storage that supports the selected clone mode.

Optional fields include `storage`, `linked_clone` (default true), `firewall`,
`cores` (default 2), `memory_mb` (default 4096), `disk_size` (for example
`32G`), and the task/address polling timeouts.

VM ownership is the exact Proxmox tag supplied by the top-level `tag` setting.
Instance IDs include both node and VMID (`node/123`). Destroy purges the VM and
its deterministically named cloud-init snippet. Use a dedicated API token with
only the clone, configure, start/stop, guest-agent read, storage snippet, and
VM deletion privileges required by the chosen pool and storage.

The provider does not force a guest architecture: clones inherit it from the
source template. An ARM-capable Proxmox environment therefore needs an ARM64
cloud-init template with its firmware and machine type already configured;
fj-bellows only applies sizing, networking, tags, and per-worker user data.

Set the top-level `worker_lifecycle` to `disposable` to make each VM single-job.
