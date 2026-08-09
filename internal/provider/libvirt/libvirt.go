// Package libvirt implements provider.Provider using virsh and cloud-localds.
package libvirt

import (
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

const (
	timeLayout            = time.RFC3339Nano
	defaultAddressTimeout = 2 * time.Minute
	defaultPollInterval   = 2 * time.Second
	architectureAuto      = "auto"
	architectureAMD64     = "x86_64"
	architectureARM64     = "aarch64"
	firmwareAuto          = "auto"
	firmwareBIOS          = "bios"
	firmwareEFI           = "efi"
	defaultVirshBin       = "virsh"
)

var (
	safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	nowUTC   = func() time.Time { return time.Now().UTC() }
)

type config struct {
	URI             string   `yaml:"uri"`
	Pool            string   `yaml:"pool"`
	BaseVolume      string   `yaml:"base_volume"`
	Network         string   `yaml:"network"`
	VirshBin        string   `yaml:"virsh_bin"`
	CloudLocalDSBin string   `yaml:"cloud_localds_bin"`
	Cores           int      `yaml:"cores"`
	MemoryMB        int      `yaml:"memory_mb"`
	DiskSize        string   `yaml:"disk_size"`
	Architecture    string   `yaml:"architecture"`
	Machine         string   `yaml:"machine"`
	Firmware        string   `yaml:"firmware"`
	AddressTimeout  duration `yaml:"address_timeout"`
	PollInterval    duration `yaml:"poll_interval"`
}

type duration time.Duration

func (d *duration) UnmarshalYAML(value *yaml.Node) error {
	v, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	*d = duration(v)
	return nil
}

func (d duration) D() time.Duration { return time.Duration(d) }

// Libvirt is a libvirt/KVM provider backed by command-line clients.
type Libvirt struct {
	cfg    config
	tag    string
	runner commandRunner
}

func init() {
	provider.Register("libvirt", func() provider.Provider { return &Libvirt{} })
}

// Configure validates provider configuration.
func (l *Libvirt) Configure(_ context.Context, tag string, node yaml.Node) error { //nolint:gocyclo // validation keeps all provider defaults and invalid combinations in one reviewable place.
	if err := node.Decode(&l.cfg); err != nil {
		return fmt.Errorf("libvirt: decode provider_config: %w", err)
	}
	var missing []string
	for key, value := range map[string]string{
		"pool": l.cfg.Pool, "base_volume": l.cfg.BaseVolume, "network": l.cfg.Network,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("libvirt: provider_config missing: %s", strings.Join(missing, ", "))
	}
	if !safeName.MatchString(tag) {
		return fmt.Errorf("libvirt: unsafe deployment tag %q", tag)
	}
	if l.cfg.URI == "" {
		l.cfg.URI = "qemu:///system"
	}
	if l.cfg.VirshBin == "" {
		l.cfg.VirshBin = defaultVirshBin
	}
	if l.cfg.CloudLocalDSBin == "" {
		l.cfg.CloudLocalDSBin = "cloud-localds"
	}
	if l.cfg.Cores == 0 {
		l.cfg.Cores = 2
	}
	if l.cfg.MemoryMB == 0 {
		l.cfg.MemoryMB = 4096
	}
	if l.cfg.Architecture == "" {
		l.cfg.Architecture = architectureAuto
	}
	if l.cfg.Firmware == "" {
		l.cfg.Firmware = firmwareAuto
	}
	if _, err := normalizeArchitecture(l.cfg.Architecture); err != nil && l.cfg.Architecture != architectureAuto {
		return err
	}
	if !safeOptionalName(l.cfg.Machine) {
		return fmt.Errorf("libvirt: unsafe machine type %q", l.cfg.Machine)
	}
	if l.cfg.Firmware != firmwareAuto && l.cfg.Firmware != firmwareBIOS && l.cfg.Firmware != firmwareEFI {
		return fmt.Errorf("libvirt: firmware must be auto, bios, or efi, got %q", l.cfg.Firmware)
	}
	if l.cfg.AddressTimeout == 0 {
		l.cfg.AddressTimeout = duration(defaultAddressTimeout)
	}
	if l.cfg.PollInterval == 0 {
		l.cfg.PollInterval = duration(defaultPollInterval)
	}
	if l.runner == nil {
		l.runner = execCommandRunner{}
	}
	l.tag = tag
	return nil
}

// Provision clones the base volume, creates a NoCloud seed, defines and starts
// a KVM domain, and waits for an address.
func (l *Libvirt) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	if !safeName.MatchString(spec.Name) {
		return provider.Instance{}, fmt.Errorf("libvirt: unsafe worker name %q", spec.Name)
	}
	platform, err := l.resolvePlatform(ctx)
	if err != nil {
		return provider.Instance{}, err
	}
	rootVolume := spec.Name + "-root.qcow2"
	seedVolume := spec.Name + "-seed.iso"
	if _, err := l.virsh(ctx, nil, "vol-clone", l.cfg.BaseVolume, rootVolume, "--pool", l.cfg.Pool); err != nil {
		return provider.Instance{}, fmt.Errorf("libvirt: clone base volume: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			l.cleanupFailedProvision(spec.Name, rootVolume, seedVolume)
		}
	}()
	if l.cfg.DiskSize != "" {
		if _, err := l.virsh(ctx, nil, "vol-resize", rootVolume, l.cfg.DiskSize, "--pool", l.cfg.Pool); err != nil {
			return provider.Instance{}, fmt.Errorf("libvirt: resize root volume: %w", err)
		}
	}
	rootPath, err := l.volumePath(ctx, rootVolume)
	if err != nil {
		return provider.Instance{}, err
	}
	seedPath, err := l.createAndUploadSeed(ctx, spec, seedVolume)
	if err != nil {
		return provider.Instance{}, err
	}
	domain, err := renderDomain(
		spec.Name, spec.Tag, rootVolume, rootPath, seedVolume, seedPath,
		l.cfg.Network, l.cfg.Cores, l.cfg.MemoryMB, platform,
	)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("libvirt: render domain XML: %w", err)
	}
	if _, err := l.virsh(ctx, domain, "define", "/dev/stdin"); err != nil {
		return provider.Instance{}, fmt.Errorf("libvirt: define domain: %w", err)
	}
	if _, err := l.virsh(ctx, nil, "start", spec.Name); err != nil {
		return provider.Instance{}, fmt.Errorf("libvirt: start domain: %w", err)
	}
	address, err := l.waitAddress(ctx, spec.Name)
	if err != nil {
		return provider.Instance{}, err
	}
	cleanup = false
	return provider.Instance{
		ID: spec.Name, Name: spec.Name, Address: address,
		CreatedAt: nowUTC(), Tag: spec.Tag,
	}, nil
}

func safeOptionalName(value string) bool {
	return value == "" || safeName.MatchString(value)
}

func normalizeArchitecture(value string) (string, error) {
	switch strings.ToLower(value) {
	case "amd64", architectureAMD64:
		return architectureAMD64, nil
	case "arm64", architectureARM64:
		return architectureARM64, nil
	default:
		return "", fmt.Errorf("libvirt: unsupported architecture %q; use auto, amd64/x86_64, or arm64/aarch64", value)
	}
}

func (l *Libvirt) resolvePlatform(ctx context.Context) (domainPlatform, error) {
	architecture := l.cfg.Architecture
	if architecture == "" || architecture == architectureAuto {
		var err error
		architecture, err = l.hostArchitecture(ctx)
		if err != nil {
			return domainPlatform{}, err
		}
	} else {
		var err error
		architecture, err = normalizeArchitecture(architecture)
		if err != nil {
			return domainPlatform{}, err
		}
	}

	machine := l.cfg.Machine
	if machine == "" && architecture == architectureARM64 {
		machine = "virt"
	}
	firmware := l.cfg.Firmware
	switch firmware {
	case "", firmwareAuto:
		if architecture == architectureARM64 {
			firmware = firmwareEFI
		} else {
			firmware = ""
		}
	case firmwareBIOS:
		firmware = ""
	}
	return domainPlatform{Architecture: architecture, Machine: machine, Firmware: firmware}, nil
}

func (l *Libvirt) hostArchitecture(ctx context.Context) (string, error) {
	out, err := l.virsh(ctx, nil, "capabilities")
	if err != nil {
		return "", fmt.Errorf("libvirt: discover host architecture: %w", err)
	}
	var capabilities struct {
		Host struct {
			CPU struct {
				Architecture string `xml:"arch"`
			} `xml:"cpu"`
		} `xml:"host"`
	}
	if err := xml.Unmarshal(out, &capabilities); err != nil {
		return "", fmt.Errorf("libvirt: decode capabilities: %w", err)
	}
	architecture, err := normalizeArchitecture(strings.TrimSpace(capabilities.Host.CPU.Architecture))
	if err != nil {
		return "", fmt.Errorf("libvirt: capabilities: %w", err)
	}
	return architecture, nil
}

func (l *Libvirt) createAndUploadSeed(ctx context.Context, spec provider.Spec, seedVolume string) (string, error) {
	tempDir, err := os.MkdirTemp("", "fj-bellows-libvirt-")
	if err != nil {
		return "", fmt.Errorf("libvirt: create seed temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	userPath := filepath.Join(tempDir, "user-data")
	metaPath := filepath.Join(tempDir, "meta-data")
	seedLocalPath := filepath.Join(tempDir, "seed.iso")
	if err := os.WriteFile(userPath, []byte(spec.UserData), 0o600); err != nil {
		return "", fmt.Errorf("libvirt: write user-data: %w", err)
	}
	meta := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", spec.Name, spec.Name)
	if err := os.WriteFile(metaPath, []byte(meta), 0o600); err != nil {
		return "", fmt.Errorf("libvirt: write meta-data: %w", err)
	}
	if _, err := l.runner.Run(ctx, nil, l.cfg.CloudLocalDSBin, seedLocalPath, userPath, metaPath); err != nil {
		return "", fmt.Errorf("libvirt: build NoCloud seed: %w", err)
	}
	info, err := os.Stat(seedLocalPath)
	if err != nil {
		return "", fmt.Errorf("libvirt: stat NoCloud seed: %w", err)
	}
	if _, err := l.virsh(ctx, nil, "vol-create-as", l.cfg.Pool, seedVolume, fmt.Sprintf("%dB", info.Size()), "--format", "raw"); err != nil {
		return "", fmt.Errorf("libvirt: create seed volume: %w", err)
	}
	if _, err := l.virsh(ctx, nil, "vol-upload", seedVolume, seedLocalPath, "--pool", l.cfg.Pool); err != nil {
		return "", fmt.Errorf("libvirt: upload seed volume: %w", err)
	}
	return l.volumePath(ctx, seedVolume)
}

func (l *Libvirt) volumePath(ctx context.Context, volume string) (string, error) {
	out, err := l.virsh(ctx, nil, "vol-path", volume, "--pool", l.cfg.Pool)
	if err != nil {
		return "", fmt.Errorf("libvirt: resolve volume %s: %w", volume, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("libvirt: volume %s has empty path", volume)
	}
	return path, nil
}

func (l *Libvirt) waitAddress(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, l.cfg.AddressTimeout.D())
	defer cancel()
	for {
		if address := l.address(ctx, id); address != "" {
			return address, nil
		}
		if err := waitContext(ctx, l.cfg.PollInterval.D()); err != nil {
			return "", fmt.Errorf("libvirt: wait for domain %s address: %w", id, err)
		}
	}
}

func (l *Libvirt) address(ctx context.Context, id string) string {
	for _, source := range []string{"agent", "lease"} {
		out, err := l.virsh(ctx, nil, "domifaddr", id, "--source", source)
		if err != nil {
			continue
		}
		if address := parseAddress(out); address != "" {
			return address
		}
	}
	return ""
}

func parseAddress(out []byte) string {
	var ipv6 string
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		candidate, _, err := net.ParseCIDR(fields[len(fields)-1])
		if err != nil || candidate.IsLoopback() || candidate.IsLinkLocalUnicast() {
			continue
		}
		if candidate.To4() != nil {
			return candidate.String()
		}
		if ipv6 == "" {
			ipv6 = candidate.String()
		}
	}
	return ipv6
}

// List returns domains whose namespaced metadata carries the exact tag.
func (l *Libvirt) List(ctx context.Context, tag string) ([]provider.Instance, error) {
	out, err := l.virsh(ctx, nil, "list", "--all", "--name")
	if err != nil {
		return nil, fmt.Errorf("libvirt: list domains: %w", err)
	}
	instances := make([]provider.Instance, 0)
	for line := range strings.Lines(string(out)) {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		domain, err := l.readDomain(ctx, name)
		if err != nil || domain.Metadata.Worker.Tag != tag {
			continue
		}
		created, _ := time.Parse(timeLayout, domain.Metadata.Worker.CreatedAt)
		instances = append(instances, provider.Instance{
			ID: name, Name: name, Address: l.address(ctx, name), CreatedAt: created, Tag: tag,
		})
	}
	return instances, nil
}

// Destroy removes the domain definition and only the volumes recorded in its
// fj-bellows metadata.
func (l *Libvirt) Destroy(ctx context.Context, id string) error {
	if !safeName.MatchString(id) {
		return fmt.Errorf("libvirt: unsafe instance ID %q", id)
	}
	domain, readErr := l.readDomain(ctx, id)
	if readErr != nil {
		return fmt.Errorf("libvirt: verify domain ownership: %w", readErr)
	}
	if domain.Metadata.Worker.Tag != l.tag {
		return fmt.Errorf("libvirt: refuse to destroy domain %q with foreign tag", id)
	}
	_, _ = l.virsh(ctx, nil, "destroy", id)
	if _, err := l.virsh(ctx, nil, "undefine", id); err != nil {
		return fmt.Errorf("libvirt: undefine domain: %w", err)
	}
	volumes := []string{domain.Metadata.Worker.RootVolume, domain.Metadata.Worker.SeedVolume}
	for _, volume := range volumes {
		if volume == "" || !safeName.MatchString(volume) {
			continue
		}
		_, _ = l.virsh(ctx, nil, "vol-delete", volume, "--pool", l.cfg.Pool)
	}
	return nil
}

func (l *Libvirt) readDomain(ctx context.Context, id string) (domainXML, error) {
	out, err := l.virsh(ctx, nil, "dumpxml", id)
	if err != nil {
		return domainXML{}, err
	}
	var domain domainXML
	if err := xml.Unmarshal(out, &domain); err != nil {
		return domainXML{}, fmt.Errorf("decode domain XML: %w", err)
	}
	return domain, nil
}

func (l *Libvirt) cleanupFailedProvision(id, rootVolume, seedVolume string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = l.virsh(ctx, nil, "destroy", id)
	_, _ = l.virsh(ctx, nil, "undefine", id)
	for _, volume := range []string{rootVolume, seedVolume} {
		_, _ = l.virsh(ctx, nil, "vol-delete", volume, "--pool", l.cfg.Pool)
	}
}

func (l *Libvirt) virsh(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	fullArgs := append([]string{"-c", l.cfg.URI}, args...)
	return l.runner.Run(ctx, stdin, l.cfg.VirshBin, fullArgs...)
}

// BillingModel reports local capacity with no external billing boundary.
func (l *Libvirt) BillingModel() provider.BillingModel { return provider.BillingPerSecond }

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
