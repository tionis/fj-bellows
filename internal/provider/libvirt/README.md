# internal/provider/libvirt

The libvirt provider creates KVM workers through `virsh` and `cloud-localds`.
It deliberately avoids the CGo libvirt binding so the main fj-bellows binary
retains its existing build and container model.

Both commands must be installed in the fj-bellows runtime environment. The
stock distroless container image does not bundle them, so containerized use
requires a derived image or host-side deployment of the binary.

Required `provider_config` fields are `pool`, `base_volume`, and `network`.
`uri` defaults to `qemu:///system`; `virsh_bin` and `cloud_localds_bin` can
override the runtime commands. Optional worker sizing fields are `cores`
(default 2), `memory_mb` (default 4096), and `disk_size`.

`architecture` defaults to `auto`, which reads the native hypervisor
architecture from `virsh capabilities`. Supported values and aliases are
`amd64`/`x86_64` and `arm64`/`aarch64`. On ARM64 the provider defaults to the
QEMU `virt` machine, requests libvirt's `efi` firmware autoselection, and
attaches the NoCloud seed as a read-only virtio disk because the ARM `virt`
machine has no default SATA controller. `machine` and `firmware` (`auto`,
`bios`, or `efi`) can be overridden for unusual hypervisors. The configured
`base_volume` must contain a cloud image built for the selected architecture.

The provider clones the base storage volume, creates and uploads a unique
NoCloud seed, defines a KVM domain, and stores its exact deployment tag plus
owned volume names in namespaced domain metadata. Address discovery tries the
QEMU guest agent first and libvirt DHCP leases second. The base image should
therefore include and enable `qemu-guest-agent` when the selected network does
not expose DHCP lease information.

Destroy reads the namespaced metadata before undefining the domain and deletes
only the two recorded provider-owned volumes. Do not point this provider at
QEMU processes managed by Proxmox VE; use the separate `proxmox` provider for
those hosts.
