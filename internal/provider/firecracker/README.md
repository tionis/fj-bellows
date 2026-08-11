# Firecracker provider

The `firecracker` provider runs disposable local microVMs from an immutable raw
root filesystem. Each provision reflink-clones the base image, creates a
read-only CIDATA NoCloud ISO
containing the ordinary fj-bellows worker bootstrap, and starts a
Firecracker VMM with KVM, virtio block, and one virtio network interface.

Host networking is intentionally outside the provider. An operator supplies a
`network_slots` list whose entries each contain a persistent TAP device, guest
address, and guest MAC. Each TAP must be attached to a NAT-capable bridge and
openable by the UID selected for that slot. Provisioning is serialized while a
free slot is reserved, so concurrent requests cannot attach two VMMs to one
TAP. The legacy scalar `tap_device`, `guest_address`, and `guest_mac` fields
remain accepted as a one-slot configuration.

For production isolation, set `jailer_bin`, `jailer_base_dir`, and
`jailer_cgroup_version`. Each network slot can name a dedicated
`jailer_user`/`jailer_group` or provide numeric `jailer_uid`/`jailer_gid`; the
global numeric UID/GID fields remain a single-slot fallback. The scheduler must
start as root because the official jailer constructs mount namespaces, device
nodes, and the chroot. Before launch the provider stages the root filesystem,
NoCloud seed, kernel, initrd, and Firecracker configuration inside the jail;
the jailer then drops the VMM to the slot's unprivileged identity. Use the
matching jailer and statically linked Firecracker binaries from the same
release.

The base must be a raw filesystem image whose kernel and optional initrd are
available separately on the host. Firecracker exposes a drive marked as the
root device as `/dev/vda`, so the default kernel command line assumes a flat
filesystem rather than a partitioned firmware boot disk. Override `boot_args`
for another compatible image layout.
Firecracker and the guest artifacts must match the host architecture.

The VMM is a child of fj-bellows. systemd therefore kills it if the scheduler
restarts; stale state and jail directories are discarded at the next list
operation and the disposable pool is replenished. Snapshots are not currently
implemented. The jailer adds important host-side defense in depth, but the
operator must still patch the host kernel and microcode, bound serial logs,
filter guest egress, and size cgroup/resource controls for the workload.
