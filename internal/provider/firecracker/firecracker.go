// Package firecracker implements provider.Provider for local Firecracker
// microVMs backed by a raw root filesystem and a persistent host TAP device.
package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

const (
	defaultFirecrackerBin = "firecracker"
	defaultJailerBin      = ""
	defaultCloudLocalDS   = "cloud-localds"
	defaultCopyBin        = "cp"
	stateFileName         = "instance.json"
	configFileName        = "firecracker.json"
	timeLayout            = time.RFC3339Nano
	defaultStartupGrace   = 3 * time.Second
	maxStartupErrorBytes  = 4096
)

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var safeJailerID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,63}$`)

type networkSlot struct {
	TapDevice    string `yaml:"tap_device"`
	GuestAddress string `yaml:"guest_address"`
	GuestMAC     string `yaml:"guest_mac"`
	JailerUID    int    `yaml:"jailer_uid,omitempty"`
	JailerGID    int    `yaml:"jailer_gid,omitempty"`
	JailerUser   string `yaml:"jailer_user,omitempty"`
	JailerGroup  string `yaml:"jailer_group,omitempty"`
}

type config struct {
	FirecrackerBin string        `yaml:"firecracker_bin"`
	JailerBin      string        `yaml:"jailer_bin"`
	JailerUID      int           `yaml:"jailer_uid"`
	JailerGID      int           `yaml:"jailer_gid"`
	JailerBaseDir  string        `yaml:"jailer_base_dir"`
	JailerCgroupV  int           `yaml:"jailer_cgroup_version"`
	CloudLocalDS   string        `yaml:"cloud_localds_bin"`
	CopyBin        string        `yaml:"copy_bin"`
	KernelImage    string        `yaml:"kernel_image"`
	InitrdImage    string        `yaml:"initrd_image"`
	BaseRootFS     string        `yaml:"base_rootfs"`
	StateDir       string        `yaml:"state_dir"`
	TapDevice      string        `yaml:"tap_device"`
	GuestAddress   string        `yaml:"guest_address"`
	GuestMAC       string        `yaml:"guest_mac"`
	NetworkSlots   []networkSlot `yaml:"network_slots"`
	VCPUCount      int           `yaml:"vcpu_count"`
	MemoryMB       int           `yaml:"memory_mb"`
	BootArgs       string        `yaml:"boot_args"`
}

type machineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemoryMB   int  `json:"mem_size_mib"`
	SMTEnabled bool `json:"smt"`
}

type bootSource struct {
	KernelImage string `json:"kernel_image_path"`
	InitrdImage string `json:"initrd_path,omitempty"`
	BootArgs    string `json:"boot_args"`
}

type drive struct {
	ID         string `json:"drive_id"`
	Path       string `json:"path_on_host"`
	RootDevice bool   `json:"is_root_device"`
	ReadOnly   bool   `json:"is_read_only"`
}

type networkInterface struct {
	ID         string `json:"iface_id"`
	GuestMAC   string `json:"guest_mac"`
	HostDevice string `json:"host_dev_name"`
}

type vmConfig struct {
	BootSource        bootSource         `json:"boot-source"`
	Drives            []drive            `json:"drives"`
	NetworkInterfaces []networkInterface `json:"network-interfaces"`
	Machine           machineConfig      `json:"machine-config"`
}

type instanceState struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Tag       string    `json:"tag"`
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
	PID       int       `json:"pid"`
	ProcStart string    `json:"proc_start"`
	TapDevice string    `json:"tap_device"`
	GuestMAC  string    `json:"guest_mac"`
	JailDir   string    `json:"jail_dir,omitempty"`
}

type managedProcess struct {
	process process
	done    chan error
}

// Firecracker manages local microVM processes. Host networking is deliberately
// prepared outside the daemon: the provider only opens a named TAP device and
// never needs CAP_NET_ADMIN.
type Firecracker struct {
	cfg          config
	tag          string
	runner       commandRunner
	launcher     processLauncher
	startupGrace time.Duration

	mu          sync.Mutex
	processes   map[string]*managedProcess
	provisionMu sync.Mutex
}

func init() {
	provider.Register("firecracker", func() provider.Provider { return &Firecracker{} })
}

// Configure validates immutable artifacts and the fixed one-slot network.
func (f *Firecracker) Configure(_ context.Context, tag string, node yaml.Node) error {
	if err := node.Decode(&f.cfg); err != nil {
		return fmt.Errorf("firecracker: decode provider_config: %w", err)
	}
	if !safeName.MatchString(tag) {
		return fmt.Errorf("firecracker: unsafe deployment tag %q", tag)
	}
	if f.cfg.FirecrackerBin == "" {
		f.cfg.FirecrackerBin = defaultFirecrackerBin
	}
	if f.cfg.JailerBin == "" {
		f.cfg.JailerBin = defaultJailerBin
	}
	if f.cfg.CloudLocalDS == "" {
		f.cfg.CloudLocalDS = defaultCloudLocalDS
	}
	if f.cfg.CopyBin == "" {
		f.cfg.CopyBin = defaultCopyBin
	}
	if f.cfg.VCPUCount == 0 {
		f.cfg.VCPUCount = 2
	}
	if f.cfg.MemoryMB == 0 {
		f.cfg.MemoryMB = 4096
	}
	if f.cfg.BootArgs == "" {
		f.cfg.BootArgs = defaultBootArgs(runtime.GOARCH)
	}
	if len(f.cfg.NetworkSlots) == 0 && (f.cfg.TapDevice != "" || f.cfg.GuestAddress != "" || f.cfg.GuestMAC != "") {
		f.cfg.NetworkSlots = []networkSlot{{
			TapDevice: f.cfg.TapDevice, GuestAddress: f.cfg.GuestAddress, GuestMAC: f.cfg.GuestMAC,
		}}
	} else if len(f.cfg.NetworkSlots) > 0 && (f.cfg.TapDevice != "" || f.cfg.GuestAddress != "" || f.cfg.GuestMAC != "") {
		return fmt.Errorf("firecracker: network_slots cannot be combined with tap_device, guest_address, or guest_mac")
	}
	var missing []string
	for key, value := range map[string]string{
		"kernel_image": f.cfg.KernelImage,
		"base_rootfs":  f.cfg.BaseRootFS,
		"state_dir":    f.cfg.StateDir,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("firecracker: provider_config missing: %s", strings.Join(missing, ", "))
	}
	if len(f.cfg.NetworkSlots) == 0 {
		return fmt.Errorf("firecracker: provider_config missing: network_slots")
	}
	if f.cfg.VCPUCount < 1 || f.cfg.MemoryMB < 128 {
		return fmt.Errorf("firecracker: vcpu_count must be positive and memory_mb at least 128")
	}
	seenTap := make(map[string]struct{}, len(f.cfg.NetworkSlots))
	seenAddress := make(map[string]struct{}, len(f.cfg.NetworkSlots))
	seenMAC := make(map[string]struct{}, len(f.cfg.NetworkSlots))
	for index, slot := range f.cfg.NetworkSlots {
		if !safeName.MatchString(slot.TapDevice) || len(slot.TapDevice) > 15 {
			return fmt.Errorf("firecracker: network_slots[%d] has invalid tap_device %q", index, slot.TapDevice)
		}
		if net.ParseIP(slot.GuestAddress) == nil {
			return fmt.Errorf("firecracker: network_slots[%d] has invalid guest_address %q", index, slot.GuestAddress)
		}
		if _, err := net.ParseMAC(slot.GuestMAC); err != nil {
			return fmt.Errorf("firecracker: network_slots[%d] has invalid guest_mac %q: %w", index, slot.GuestMAC, err)
		}
		if f.cfg.JailerBin != "" {
			if err := resolveJailerIdentity(&slot); err != nil {
				return fmt.Errorf("firecracker: network_slots[%d]: %w", index, err)
			}
			f.cfg.NetworkSlots[index] = slot
			uid, gid := f.jailerIdentity(slot)
			if uid <= 0 || gid <= 0 {
				return fmt.Errorf("firecracker: network_slots[%d] requires positive jailer_uid and jailer_gid", index)
			}
		}
		for label, candidate := range map[string]struct {
			value string
			seen  map[string]struct{}
		}{
			"tap_device":    {slot.TapDevice, seenTap},
			"guest_address": {slot.GuestAddress, seenAddress},
			"guest_mac":     {strings.ToLower(slot.GuestMAC), seenMAC},
		} {
			if _, exists := candidate.seen[candidate.value]; exists {
				return fmt.Errorf("firecracker: duplicate %s %q in network_slots", label, candidate.value)
			}
			candidate.seen[candidate.value] = struct{}{}
		}
	}
	f.cfg.TapDevice = f.cfg.NetworkSlots[0].TapDevice
	f.cfg.GuestAddress = f.cfg.NetworkSlots[0].GuestAddress
	f.cfg.GuestMAC = f.cfg.NetworkSlots[0].GuestMAC
	for label, path := range map[string]string{
		"kernel_image": f.cfg.KernelImage,
		"base_rootfs":  f.cfg.BaseRootFS,
	} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("firecracker: %s must be an absolute path", label)
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("firecracker: %s is not a regular file: %s", label, path)
		}
	}
	if f.cfg.InitrdImage != "" {
		if !filepath.IsAbs(f.cfg.InitrdImage) {
			return fmt.Errorf("firecracker: initrd_image must be an absolute path")
		}
		if info, err := os.Stat(f.cfg.InitrdImage); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("firecracker: initrd_image is not a regular file: %s", f.cfg.InitrdImage)
		}
	}
	if !filepath.IsAbs(f.cfg.StateDir) || filepath.Clean(f.cfg.StateDir) == "/" {
		return fmt.Errorf("firecracker: state_dir must be an absolute, non-root path")
	}
	if f.cfg.JailerBin != "" {
		if f.cfg.JailerBaseDir == "" {
			return fmt.Errorf("firecracker: jailer_base_dir is required when jailer_bin is set")
		}
		if !filepath.IsAbs(f.cfg.JailerBin) || !filepath.IsAbs(f.cfg.FirecrackerBin) {
			return fmt.Errorf("firecracker: jailer_bin and firecracker_bin must be absolute paths when jailer is enabled")
		}
		if !filepath.IsAbs(f.cfg.JailerBaseDir) || filepath.Clean(f.cfg.JailerBaseDir) == "/" {
			return fmt.Errorf("firecracker: jailer_base_dir must be an absolute, non-root path")
		}
		if f.cfg.JailerCgroupV == 0 {
			f.cfg.JailerCgroupV = 2
		}
		if f.cfg.JailerCgroupV != 1 && f.cfg.JailerCgroupV != 2 {
			return fmt.Errorf("firecracker: jailer_cgroup_version must be 1 or 2")
		}
		if err := os.MkdirAll(f.cfg.JailerBaseDir, 0o700); err != nil {
			return fmt.Errorf("firecracker: create jailer_base_dir: %w", err)
		}
	}
	if err := os.MkdirAll(f.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("firecracker: create state_dir: %w", err)
	}
	if f.runner == nil {
		f.runner = execCommandRunner{}
	}
	if f.launcher == nil {
		f.launcher = execProcessLauncher{}
	}
	if f.processes == nil {
		f.processes = make(map[string]*managedProcess)
	}
	if f.startupGrace == 0 {
		f.startupGrace = defaultStartupGrace
	}
	f.tag = tag
	return nil
}

func (f *Firecracker) jailerIdentity(slot networkSlot) (int, int) {
	uid, gid := slot.JailerUID, slot.JailerGID
	if uid == 0 {
		uid = f.cfg.JailerUID
	}
	if gid == 0 {
		gid = f.cfg.JailerGID
	}
	return uid, gid
}

func resolveJailerIdentity(slot *networkSlot) error {
	if slot.JailerUID == 0 && slot.JailerUser != "" {
		account, err := user.Lookup(slot.JailerUser)
		if err != nil {
			return fmt.Errorf("lookup jailer_user %q: %w", slot.JailerUser, err)
		}
		uid, err := strconv.Atoi(account.Uid)
		if err != nil {
			return fmt.Errorf("parse UID for jailer_user %q: %w", slot.JailerUser, err)
		}
		slot.JailerUID = uid
		if slot.JailerGID == 0 && slot.JailerGroup == "" {
			gid, parseErr := strconv.Atoi(account.Gid)
			if parseErr != nil {
				return fmt.Errorf("parse primary GID for jailer_user %q: %w", slot.JailerUser, parseErr)
			}
			slot.JailerGID = gid
		}
	}
	if slot.JailerGID == 0 && slot.JailerGroup != "" {
		group, err := user.LookupGroup(slot.JailerGroup)
		if err != nil {
			return fmt.Errorf("lookup jailer_group %q: %w", slot.JailerGroup, err)
		}
		gid, err := strconv.Atoi(group.Gid)
		if err != nil {
			return fmt.Errorf("parse GID for jailer_group %q: %w", slot.JailerGroup, err)
		}
		slot.JailerGID = gid
	}
	return nil
}

func defaultBootArgs(architecture string) string {
	args := "console=ttyS0 reboot=k panic=1 root=/dev/vda rw"
	if architecture == "amd64" {
		args += " pci=off"
	} else if architecture == "arm64" {
		args = "keep_bootcon " + args
	}
	return args
}

// Provision reflink-clones the immutable raw base, creates an ext4 NoCloud
// seed, reserves one configured network slot, then launches Firecracker either
// directly or through the jailer. Provisioning is serialized so a TAP can
// never be assigned to two concurrent VMMs.
func (f *Firecracker) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) { //nolint:gocyclo // ordered rollback is easier to audit in one lifecycle function.
	if !safeName.MatchString(spec.Name) {
		return provider.Instance{}, fmt.Errorf("firecracker: unsafe worker name %q", spec.Name)
	}
	if spec.Tag != f.tag {
		return provider.Instance{}, fmt.Errorf("firecracker: refuse to provision foreign tag %q", spec.Tag)
	}
	if f.cfg.JailerBin != "" && !safeJailerID.MatchString(spec.Name) {
		return provider.Instance{}, fmt.Errorf("firecracker: worker name %q is not a valid jailer ID", spec.Name)
	}
	f.provisionMu.Lock()
	defer f.provisionMu.Unlock()
	instances, err := f.List(ctx, spec.Tag)
	if err != nil {
		return provider.Instance{}, err
	}
	usedTAPs := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		state, stateErr := f.readState(instance.ID)
		if stateErr != nil {
			continue
		}
		if state.TapDevice != "" {
			usedTAPs[state.TapDevice] = struct{}{}
			continue
		}
		for _, candidate := range f.cfg.NetworkSlots {
			if candidate.GuestAddress == state.Address {
				usedTAPs[candidate.TapDevice] = struct{}{}
			}
		}
	}
	var slot networkSlot
	for _, candidate := range f.cfg.NetworkSlots {
		if _, used := usedTAPs[candidate.TapDevice]; !used {
			slot = candidate
			break
		}
	}
	if slot.TapDevice == "" {
		return provider.Instance{}, fmt.Errorf("firecracker: all %d network slots are in use", len(f.cfg.NetworkSlots))
	}
	dir := filepath.Join(f.cfg.StateDir, spec.Name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return provider.Instance{}, fmt.Errorf("firecracker: create instance directory: %w", err)
	}
	cleanup := true
	jailDir := ""
	jailRoot := ""
	if f.cfg.JailerBin != "" {
		jailDir = filepath.Join(f.cfg.JailerBaseDir, filepath.Base(f.cfg.FirecrackerBin), spec.Name)
		jailRoot = filepath.Join(jailDir, "root")
		if err := os.MkdirAll(jailRoot, 0o700); err != nil {
			return provider.Instance{}, fmt.Errorf("firecracker: create jail root: %w", err)
		}
	}
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
			if jailDir != "" {
				_ = os.RemoveAll(jailDir)
			}
		}
	}()

	rootPath := filepath.Join(dir, "rootfs.raw")
	if jailRoot != "" {
		rootPath = filepath.Join(jailRoot, "rootfs.raw")
	}
	if _, err := f.runner.Run(ctx, f.cfg.CopyBin, "--reflink=always", "--sparse=auto", f.cfg.BaseRootFS, rootPath); err != nil {
		return provider.Instance{}, fmt.Errorf("firecracker: clone rootfs: %w", err)
	}
	seedPath, err := f.createSeed(ctx, dir, spec)
	if err != nil {
		return provider.Instance{}, err
	}
	configPath := filepath.Join(dir, configFileName)
	kernelPath := f.cfg.KernelImage
	initrdPath := f.cfg.InitrdImage
	launchCommand := f.cfg.FirecrackerBin
	launchArgs := []string{"--no-api", "--config-file", configPath}
	if f.cfg.JailerBin != "" {
		jailerUID, jailerGID := f.jailerIdentity(slot)
		if err := os.Chmod(rootPath, 0o600); err != nil {
			return provider.Instance{}, fmt.Errorf("firecracker: chmod jailed rootfs: %w", err)
		}
		if err := os.Chown(rootPath, jailerUID, jailerGID); err != nil {
			return provider.Instance{}, fmt.Errorf("firecracker: chown jailed rootfs: %w", err)
		}
		seedPath, err = moveIntoJail(seedPath, filepath.Join(jailRoot, "seed.iso"), jailerUID, jailerGID, 0o400)
		if err != nil {
			return provider.Instance{}, err
		}
		kernelPath = filepath.Join(jailRoot, "kernel")
		if err := copyIntoJail(f.cfg.KernelImage, kernelPath, jailerUID, jailerGID, 0o400); err != nil {
			return provider.Instance{}, err
		}
		if f.cfg.InitrdImage != "" {
			initrdPath = filepath.Join(jailRoot, "initrd.img")
			if err := copyIntoJail(f.cfg.InitrdImage, initrdPath, jailerUID, jailerGID, 0o400); err != nil {
				return provider.Instance{}, err
			}
		}
		configPath = filepath.Join(jailRoot, configFileName)
		rootPath = "/rootfs.raw"
		seedPath = "/seed.iso"
		kernelPath = "/kernel"
		if initrdPath != "" {
			initrdPath = "/initrd.img"
		}
		launchCommand = f.cfg.JailerBin
		launchArgs = []string{
			"--id", spec.Name,
			"--exec-file", f.cfg.FirecrackerBin,
			"--uid", strconv.Itoa(jailerUID),
			"--gid", strconv.Itoa(jailerGID),
			"--chroot-base-dir", f.cfg.JailerBaseDir,
			"--cgroup-version", strconv.Itoa(f.cfg.JailerCgroupV),
			"--", "--no-api", "--config-file", "/" + configFileName,
		}
	}
	if err := writeJSON(configPath, vmConfig{
		BootSource: bootSource{KernelImage: kernelPath, InitrdImage: initrdPath, BootArgs: f.cfg.BootArgs},
		Drives: []drive{
			{ID: "rootfs", Path: rootPath, RootDevice: true, ReadOnly: false},
			{ID: "seed", Path: seedPath, RootDevice: false, ReadOnly: true},
		},
		NetworkInterfaces: []networkInterface{{ID: "eth0", GuestMAC: slot.GuestMAC, HostDevice: slot.TapDevice}},
		Machine:           machineConfig{VCPUCount: f.cfg.VCPUCount, MemoryMB: f.cfg.MemoryMB, SMTEnabled: false},
	}); err != nil {
		return provider.Instance{}, err
	}
	if f.cfg.JailerBin != "" {
		jailerUID, jailerGID := f.jailerIdentity(slot)
		if err := os.Chown(configPath, jailerUID, jailerGID); err != nil {
			return provider.Instance{}, fmt.Errorf("firecracker: chown jailed config: %w", err)
		}
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "console.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("firecracker: open console log: %w", err)
	}
	defer logFile.Close()
	proc, err := f.launcher.Start(launchCommand, launchArgs, logFile, logFile)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("firecracker: start microVM: %w", err)
	}
	managed := &managedProcess{process: proc, done: make(chan error, 1)}
	go func() { managed.done <- proc.Wait() }()
	select {
	case waitErr := <-managed.done:
		_ = logFile.Sync()
		detail := readTail(filepath.Join(dir, "console.log"), maxStartupErrorBytes)
		return provider.Instance{}, fmt.Errorf("firecracker: VMM exited during startup: %v: %s", waitErr, detail)
	case <-ctx.Done():
		_ = proc.Signal(syscall.SIGKILL)
		return provider.Instance{}, fmt.Errorf("firecracker: startup canceled: %w", ctx.Err())
	case <-time.After(f.startupGrace):
	}
	created := time.Now().UTC()
	state := instanceState{
		ID: spec.Name, Name: spec.Name, Tag: spec.Tag, Address: slot.GuestAddress,
		CreatedAt: created, PID: proc.PID(), ProcStart: readProcStart(proc.PID()),
		TapDevice: slot.TapDevice, GuestMAC: slot.GuestMAC, JailDir: jailDir,
	}
	if err := writeJSON(filepath.Join(dir, stateFileName), state); err != nil {
		_ = proc.Signal(syscall.SIGKILL)
		return provider.Instance{}, err
	}
	f.mu.Lock()
	f.processes[spec.Name] = managed
	f.mu.Unlock()
	cleanup = false
	return provider.Instance{ID: spec.Name, Name: spec.Name, Address: slot.GuestAddress, CreatedAt: created, Tag: spec.Tag}, nil
}

func moveIntoJail(source, destination string, uid, gid int, mode os.FileMode) (string, error) {
	if err := os.Rename(source, destination); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return "", fmt.Errorf("firecracker: stage %s in jail: %w", filepath.Base(source), err)
		}
		if err := copyIntoJail(source, destination, uid, gid, mode); err != nil {
			return "", err
		}
		if err := os.Remove(source); err != nil {
			return "", fmt.Errorf("firecracker: remove staged %s: %w", filepath.Base(source), err)
		}
		return destination, nil
	}
	if err := os.Chmod(destination, mode); err != nil {
		return "", fmt.Errorf("firecracker: chmod jailed %s: %w", filepath.Base(destination), err)
	}
	if err := os.Chown(destination, uid, gid); err != nil {
		return "", fmt.Errorf("firecracker: chown jailed %s: %w", filepath.Base(destination), err)
	}
	return destination, nil
}

func copyIntoJail(source, destination string, uid, gid int, mode os.FileMode) error {
	input, err := os.Open(source) //nolint:gosec // trusted operator-configured artifact.
	if err != nil {
		return fmt.Errorf("firecracker: open jail input %s: %w", filepath.Base(source), err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("firecracker: create jailed %s: %w", filepath.Base(destination), err)
	}
	if _, err = io.Copy(output, input); err == nil {
		err = output.Sync()
	}
	closeErr := output.Close()
	if err != nil {
		return fmt.Errorf("firecracker: copy jailed %s: %w", filepath.Base(destination), err)
	}
	if closeErr != nil {
		return fmt.Errorf("firecracker: close jailed %s: %w", filepath.Base(destination), closeErr)
	}
	if err := os.Chown(destination, uid, gid); err != nil {
		return fmt.Errorf("firecracker: chown jailed %s: %w", filepath.Base(destination), err)
	}
	return nil
}

func (f *Firecracker) createSeed(ctx context.Context, dir string, spec provider.Spec) (string, error) {
	userPath := filepath.Join(dir, "user-data")
	metaPath := filepath.Join(dir, "meta-data")
	seedPath := filepath.Join(dir, "seed.iso")
	if err := os.WriteFile(userPath, []byte(spec.UserData), 0o600); err != nil {
		return "", fmt.Errorf("firecracker: write user-data: %w", err)
	}
	meta := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", spec.Name, spec.Name)
	if err := os.WriteFile(metaPath, []byte(meta), 0o600); err != nil {
		return "", fmt.Errorf("firecracker: write meta-data: %w", err)
	}
	if _, err := f.runner.Run(ctx, f.cfg.CloudLocalDS, seedPath, userPath, metaPath); err != nil {
		return "", fmt.Errorf("firecracker: build NoCloud seed: %w", err)
	}
	_ = os.Remove(userPath)
	_ = os.Remove(metaPath)
	return seedPath, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("firecracker: encode %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("firecracker: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readTail(path string, limit int) string {
	data, err := os.ReadFile(path) //nolint:gosec // provider-owned console path.
	if err != nil {
		return "console unavailable"
	}
	if len(data) > limit {
		data = data[len(data)-limit:]
	}
	detail := strings.TrimSpace(string(data))
	if detail == "" {
		return "console empty"
	}
	return detail
}

// List returns only live processes carrying the exact deployment tag. Stale
// state from a daemon restart is removed because systemd kills child VMMs with
// the scheduler's cgroup.
func (f *Firecracker) List(_ context.Context, tag string) ([]provider.Instance, error) {
	entries, err := os.ReadDir(f.cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("firecracker: list state: %w", err)
	}
	instances := make([]provider.Instance, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !safeName.MatchString(entry.Name()) {
			continue
		}
		state, err := f.readState(entry.Name())
		if err != nil || state.Tag != tag || state.ID != entry.Name() {
			continue
		}
		if !f.isAlive(state) {
			f.removeInstanceFiles(state)
			continue
		}
		instances = append(instances, provider.Instance{
			ID: state.ID, Name: state.Name, Address: state.Address,
			CreatedAt: state.CreatedAt, Tag: state.Tag,
		})
	}
	return instances, nil
}

// Destroy terminates the owned VMM and removes its disposable rootfs and seed.
func (f *Firecracker) Destroy(ctx context.Context, id string) error {
	if !safeName.MatchString(id) {
		return fmt.Errorf("firecracker: unsafe instance ID %q", id)
	}
	state, err := f.readState(id)
	if err != nil {
		return fmt.Errorf("firecracker: verify instance ownership: %w", err)
	}
	if state.Tag != f.tag || state.ID != id {
		return fmt.Errorf("firecracker: refuse to destroy instance %q with foreign ownership", id)
	}
	f.mu.Lock()
	managed := f.processes[id]
	f.mu.Unlock()
	if managed != nil {
		_ = managed.process.Signal(syscall.SIGTERM)
		select {
		case <-managed.done:
		case <-ctx.Done():
			_ = managed.process.Signal(syscall.SIGKILL)
			return fmt.Errorf("firecracker: stop instance %s: %w", id, ctx.Err())
		case <-time.After(10 * time.Second):
			_ = managed.process.Signal(syscall.SIGKILL)
			select {
			case <-managed.done:
			case <-time.After(2 * time.Second):
				return fmt.Errorf("firecracker: instance %s did not exit", id)
			}
		}
	} else if f.isAlive(state) {
		proc, findErr := os.FindProcess(state.PID)
		if findErr != nil {
			return fmt.Errorf("firecracker: find process %d: %w", state.PID, findErr)
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("firecracker: stop process %d: %w", state.PID, err)
		}
	}
	f.mu.Lock()
	delete(f.processes, id)
	f.mu.Unlock()
	f.removeInstanceFiles(state)
	return nil
}

func (f *Firecracker) removeInstanceFiles(state instanceState) {
	_ = os.RemoveAll(filepath.Join(f.cfg.StateDir, state.ID))
	if state.JailDir == "" || f.cfg.JailerBaseDir == "" {
		return
	}
	base := filepath.Clean(f.cfg.JailerBaseDir) + string(os.PathSeparator)
	jail := filepath.Clean(state.JailDir)
	if strings.HasPrefix(jail+string(os.PathSeparator), base) {
		_ = os.RemoveAll(jail)
	}
}

func (f *Firecracker) readState(id string) (instanceState, error) {
	path := filepath.Join(f.cfg.StateDir, id, stateFileName)
	data, err := os.ReadFile(path) //nolint:gosec // id is strictly validated before path construction.
	if err != nil {
		return instanceState{}, err
	}
	var state instanceState
	if err := json.Unmarshal(data, &state); err != nil {
		return instanceState{}, fmt.Errorf("decode state: %w", err)
	}
	return state, nil
}

func (f *Firecracker) isAlive(state instanceState) bool {
	f.mu.Lock()
	managed := f.processes[state.ID]
	f.mu.Unlock()
	if managed != nil {
		select {
		case <-managed.done:
			return false
		default:
			return true
		}
	}
	return state.PID > 0 && state.ProcStart != "" && readProcStart(state.PID) == state.ProcStart && processExecutable(state.PID, f.cfg.FirecrackerBin)
}

func readProcStart(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	// comm is parenthesized and may contain spaces; starttime is field 22.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func processExecutable(pid int, configured string) bool {
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	configuredBase := filepath.Base(configured)
	return filepath.Base(strings.TrimSuffix(target, " (deleted)")) == configuredBase
}

// BillingModel reports local capacity with no external billing boundary.
func (f *Firecracker) BillingModel() provider.BillingModel { return provider.BillingPerSecond }

// Info exposes non-secret runtime details through the control plane.
func (f *Firecracker) Info(_ context.Context) map[string]string {
	info := map[string]string{
		"tap_device":    f.cfg.TapDevice,
		"guest_address": f.cfg.GuestAddress,
		"network_slots": strconv.Itoa(len(f.cfg.NetworkSlots)),
		"vcpu_count":    strconv.Itoa(f.cfg.VCPUCount),
		"memory_mb":     strconv.Itoa(f.cfg.MemoryMB),
	}
	if f.cfg.JailerBin != "" {
		info["jailer"] = "enabled"
	} else {
		info["jailer"] = "disabled"
	}
	return info
}
