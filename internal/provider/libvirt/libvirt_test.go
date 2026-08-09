package libvirt

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

const (
	testTag        = "test-pool"
	testWorkerName = "worker-1"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	run   func(stdin []byte, command string, args ...string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, command string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{command}, args...))
	f.mu.Unlock()
	return f.run(stdin, command, args...)
}

func configuredProvider(runner commandRunner) *Libvirt {
	return &Libvirt{
		cfg: config{
			URI: "qemu:///system", Pool: "workers", BaseVolume: "debian-base.qcow2",
			Network: "ciworkers", VirshBin: defaultVirshBin, CloudLocalDSBin: "cloud-localds",
			Cores: 2, MemoryMB: 2048, Architecture: architectureAMD64, Firmware: firmwareAuto,
			AddressTimeout: duration(time.Second), PollInterval: duration(time.Millisecond),
		},
		tag: testTag, runner: runner,
	}
}

func stripConnectionArgs(args []string) []string {
	if len(args) >= 2 && args[0] == "-c" {
		return args[2:]
	}
	return args
}

func TestConfigureDefaults(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("pool: workers\nbase_volume: base.qcow2\nnetwork: ci\n"), &node); err != nil {
		t.Fatal(err)
	}
	l := &Libvirt{runner: &fakeRunner{}}
	if err := l.Configure(t.Context(), "pool", node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if l.cfg.URI != "qemu:///system" || l.cfg.Cores != 2 || l.cfg.MemoryMB != 4096 ||
		l.cfg.Architecture != architectureAuto || l.cfg.Firmware != firmwareAuto {
		t.Errorf("defaults = %+v", l.cfg)
	}
}

func TestARM64PlatformDiscoveryAndDomain(t *testing.T) { //nolint:gocyclo // one test validates discovery and every coupled ARM domain choice.
	runner := &fakeRunner{run: func(_ []byte, command string, args ...string) ([]byte, error) {
		if command != defaultVirshBin {
			return nil, fmt.Errorf("unexpected command %s", command)
		}
		args = stripConnectionArgs(args)
		if len(args) == 1 && args[0] == "capabilities" {
			return []byte(`<capabilities><host><cpu><arch>aarch64</arch></cpu></host></capabilities>`), nil
		}
		return nil, fmt.Errorf("unexpected command: %v", args)
	}}
	l := configuredProvider(runner)
	l.cfg.Architecture = architectureAuto

	platform, err := l.resolvePlatform(t.Context())
	if err != nil {
		t.Fatalf("resolvePlatform: %v", err)
	}
	if platform.Architecture != architectureARM64 || platform.Machine != "virt" || platform.Firmware != firmwareEFI {
		t.Fatalf("platform = %+v", platform)
	}
	domainBytes, err := renderDomain(
		testWorkerName, testTag, "root.qcow2", "/images/root", "seed.iso", "/images/seed",
		"ciworkers", 2, 2048, platform,
	)
	if err != nil {
		t.Fatalf("renderDomain: %v", err)
	}
	var domain domainXML
	if err := xml.Unmarshal(domainBytes, &domain); err != nil {
		t.Fatalf("decode domain: %v", err)
	}
	if domain.OS.Firmware != firmwareEFI || domain.OS.Type.Architecture != architectureARM64 || domain.OS.Type.Machine != "virt" {
		t.Errorf("domain OS = %+v", domain.OS)
	}
	seed := domain.Devices.Disks[1]
	if seed.Device != "disk" || seed.Target.Bus != deviceBusVirtio || seed.Target.Dev != "vdb" || seed.ReadOnly == nil {
		t.Errorf("ARM64 seed disk = %+v", seed)
	}
}

func TestNormalizeArchitectureAliases(t *testing.T) {
	for input, want := range map[string]string{
		"amd64": architectureAMD64, architectureAMD64: architectureAMD64,
		"arm64": architectureARM64, architectureARM64: architectureARM64,
	} {
		got, err := normalizeArchitecture(input)
		if err != nil || got != want {
			t.Errorf("normalizeArchitecture(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := normalizeArchitecture("riscv64"); err == nil {
		t.Fatal("normalizeArchitecture(riscv64) succeeded")
	}
}

func TestProvisionBuildsOwnedDomain(t *testing.T) { //nolint:gocyclo // command fake validates the complete amd64 provision workflow.
	runner := &fakeRunner{}
	runner.run = func(stdin []byte, command string, args ...string) ([]byte, error) {
		if command == "cloud-localds" {
			if err := os.WriteFile(args[0], []byte("seed"), 0o600); err != nil {
				return nil, err
			}
			return nil, nil
		}
		args = stripConnectionArgs(args)
		switch args[0] {
		case "vol-clone", "vol-create-as", "vol-upload", "start":
			return nil, nil
		case "vol-path":
			return []byte("/var/lib/libvirt/images/" + args[1] + "\n"), nil
		case "define":
			var domain domainXML
			if err := xml.Unmarshal(stdin, &domain); err != nil {
				return nil, err
			}
			if domain.Metadata.Worker.Tag != testTag || domain.Metadata.Worker.RootVolume == "" {
				return nil, fmt.Errorf("bad worker metadata: %+v", domain.Metadata.Worker)
			}
			if domain.OS.Type.Architecture != architectureAMD64 || domain.OS.Firmware != "" {
				return nil, fmt.Errorf("bad amd64 OS config: %+v", domain.OS)
			}
			seed := domain.Devices.Disks[1]
			if seed.Device != "cdrom" || seed.Target.Bus != "sata" || seed.Target.Dev != "sda" {
				return nil, fmt.Errorf("bad amd64 seed disk: %+v", seed)
			}
			return nil, nil
		case "domifaddr":
			return []byte("vnet0 52:54:00:00:00:01 ipv6 fdf0::10/64\n"), nil
		default:
			return nil, fmt.Errorf("unexpected command: %v", args)
		}
	}
	l := configuredProvider(runner)

	inst, err := l.Provision(t.Context(), provider.Spec{
		Tag: testTag, Name: testWorkerName, UserData: "#cloud-config\n",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if inst.ID != testWorkerName || inst.Address != "fdf0::10" || inst.Tag != testTag {
		t.Errorf("instance = %+v", inst)
	}
}

func TestListFiltersMetadataAndDestroyUsesRecordedVolumes(t *testing.T) { //nolint:gocyclo // command fake intentionally validates the full ownership lifecycle.
	domain, err := renderDomain(
		testWorkerName, testTag, "owned-root.qcow2", "/images/root", "owned-seed.iso", "/images/seed",
		"ciworkers", 2, 2048, domainPlatform{Architecture: architectureAMD64},
	)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(_ []byte, command string, args ...string) ([]byte, error) {
		if command != defaultVirshBin {
			return nil, fmt.Errorf("unexpected command %s", command)
		}
		args = stripConnectionArgs(args)
		switch args[0] {
		case "list":
			return []byte(testWorkerName + "\nforeign\n"), nil
		case "dumpxml":
			if args[1] == testWorkerName {
				return domain, nil
			}
			return []byte(`<domain type="kvm"><name>foreign</name><metadata><worker xmlns="https://fj-bellows.invalid/xmlns/worker/1" tag="other"/></metadata></domain>`), nil
		case "domifaddr":
			return []byte("vnet0 52:54:00:00:00:01 ipv4 192.0.2.20/24\n"), nil
		case "destroy", "undefine", "vol-delete":
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected command: %v", args)
		}
	}
	l := configuredProvider(runner)

	instances, err := l.List(context.Background(), testTag)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(instances) != 1 || instances[0].ID != testWorkerName {
		t.Fatalf("instances = %+v", instances)
	}
	if err := l.Destroy(context.Background(), testWorkerName); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	var deleted []string
	for _, call := range runner.calls {
		args := stripConnectionArgs(call[1:])
		if len(args) >= 2 && args[0] == "vol-delete" {
			deleted = append(deleted, args[1])
		}
	}
	slices.Sort(deleted)
	if strings.Join(deleted, ",") != "owned-root.qcow2,owned-seed.iso" {
		t.Errorf("deleted volumes = %v", deleted)
	}
}

func TestParseAddressPrefersIPv4(t *testing.T) {
	out := []byte("vnet0 mac ipv6 fdf0::20/64\nvnet0 mac ipv4 192.0.2.20/24\n")
	if got := parseAddress(out); got != "192.0.2.20" {
		t.Errorf("parseAddress = %q", got)
	}
}

func TestDestroyRefusesWhenOwnershipCannotBeRead(t *testing.T) {
	runner := &fakeRunner{run: func(_ []byte, _ string, args ...string) ([]byte, error) {
		args = stripConnectionArgs(args)
		if args[0] == "dumpxml" {
			return nil, errors.New("permission denied")
		}
		return nil, fmt.Errorf("unexpected destructive command: %v", args)
	}}
	l := configuredProvider(runner)

	if err := l.Destroy(t.Context(), testWorkerName); err == nil || !strings.Contains(err.Error(), "verify domain ownership") {
		t.Fatalf("Destroy error = %v, want ownership verification failure", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("commands = %v, want only dumpxml", runner.calls)
	}
}
