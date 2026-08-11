package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

type fakeRunner struct{ calls [][]string }

func (r *fakeRunner) Run(_ context.Context, command string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	switch command {
	case "cp-test":
		data, err := os.ReadFile(args[len(args)-2])
		if err != nil {
			return nil, err
		}
		return nil, os.WriteFile(args[len(args)-1], data, 0o600)
	case "cloud-localds-test":
		return nil, os.WriteFile(args[0], []byte("seed"), 0o600)
	default:
		return nil, fmt.Errorf("unexpected command %s", command)
	}
}

type fakeProcess struct {
	pid  int
	once sync.Once
	done chan struct{}
}

func (p *fakeProcess) PID() int { return p.pid }

func (p *fakeProcess) Signal(_ os.Signal) error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *fakeProcess) Wait() error {
	<-p.done
	return nil
}

type fakeLauncher struct {
	command   string
	args      []string
	process   *fakeProcess
	processes []*fakeProcess
	calls     []launcherCall
}

type launcherCall struct {
	command string
	args    []string
}

func (l *fakeLauncher) Start(command string, args []string, _, _ io.Writer) (process, error) {
	l.command = command
	l.args = append([]string(nil), args...)
	l.calls = append(l.calls, launcherCall{command: command, args: append([]string(nil), args...)})
	if len(l.processes) > 0 {
		process := l.processes[0]
		l.processes = l.processes[1:]
		return process, nil
	}
	return l.process, nil
}

func configuredFirecracker(t *testing.T) (*Firecracker, string) {
	t.Helper()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	root := filepath.Join(dir, "base.raw")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "instances")
	yamlConfig := fmt.Sprintf(`
firecracker_bin: firecracker-test
cloud_localds_bin: cloud-localds-test
copy_bin: cp-test
kernel_image: %s
base_rootfs: %s
state_dir: %s
tap_device: tap-test
guest_address: 192.0.2.10
guest_mac: "02:fc:00:00:00:10"
vcpu_count: 2
memory_mb: 2048
boot_args: console=ttyS0 root=/dev/vda1 rw
`, kernel, root, stateDir)
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(yamlConfig), &node); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	launcher := &fakeLauncher{process: &fakeProcess{pid: os.Getpid(), done: make(chan struct{})}}
	f := &Firecracker{runner: runner, launcher: launcher, startupGrace: time.Millisecond}
	if err := f.Configure(t.Context(), "test-pool", node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return f, stateDir
}

func TestConfigureDefaultsAndValidation(t *testing.T) {
	f, _ := configuredFirecracker(t)
	if f.cfg.VCPUCount != 2 || f.cfg.MemoryMB != 2048 {
		t.Fatalf("config = %+v", f.cfg)
	}
	if f.BillingModel() != provider.BillingPerSecond {
		t.Fatalf("BillingModel = %v", f.BillingModel())
	}
	info := f.Info(t.Context())
	if info["tap_device"] != "tap-test" || info["guest_address"] != "192.0.2.10" {
		t.Fatalf("Info = %v", info)
	}
}

func TestDefaultBootArgsUseFlatRootDevice(t *testing.T) {
	tests := map[string]string{
		"amd64": "console=ttyS0 reboot=k panic=1 root=/dev/vda rw pci=off",
		"arm64": "keep_bootcon console=ttyS0 reboot=k panic=1 root=/dev/vda rw",
	}
	for architecture, want := range tests {
		if got := defaultBootArgs(architecture); got != want {
			t.Errorf("defaultBootArgs(%q) = %q, want %q", architecture, got, want)
		}
	}
}

func TestProvisionListDestroy(t *testing.T) {
	f, stateDir := configuredFirecracker(t)
	inst, err := f.Provision(t.Context(), provider.Spec{
		Tag: "test-pool", Name: "worker-1", UserData: "#cloud-config\n",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if inst.Address != "192.0.2.10" || inst.ID != "worker-1" {
		t.Fatalf("instance = %+v", inst)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "worker-1", configFileName))
	if err != nil {
		t.Fatal(err)
	}
	var got vmConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Machine.MemoryMB != 2048 || got.NetworkInterfaces[0].HostDevice != "tap-test" || len(got.Drives) != 2 {
		t.Fatalf("VM config = %+v", got)
	}
	instances, err := f.List(t.Context(), "test-pool")
	if err != nil || len(instances) != 1 {
		t.Fatalf("List = %+v, %v", instances, err)
	}
	if _, err := f.Provision(t.Context(), provider.Spec{Tag: "test-pool", Name: "worker-2"}); err == nil {
		t.Fatal("second Provision succeeded on fixed TAP")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.Destroy(ctx, "worker-1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "worker-1")); !os.IsNotExist(err) {
		t.Fatalf("instance directory remains: %v", err)
	}
}

func TestDestroyRefusesForeignState(t *testing.T) {
	f, stateDir := configuredFirecracker(t)
	dir := filepath.Join(stateDir, "foreign")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, stateFileName), instanceState{ID: "foreign", Tag: "another-pool"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Destroy(t.Context(), "foreign"); err == nil {
		t.Fatal("Destroy accepted foreign state")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("foreign state changed: %v", err)
	}
}

func TestProvisionUsesDistinctNetworkSlots(t *testing.T) {
	f, stateDir := configuredFirecracker(t)
	f.cfg.NetworkSlots = append(f.cfg.NetworkSlots, networkSlot{
		TapDevice: "tap-test-2", GuestAddress: "192.0.2.11", GuestMAC: "02:fc:00:00:00:11",
	})
	f.launcher = &fakeLauncher{processes: []*fakeProcess{
		{pid: os.Getpid(), done: make(chan struct{})},
		{pid: os.Getpid(), done: make(chan struct{})},
	}}

	first, err := f.Provision(t.Context(), provider.Spec{Tag: "test-pool", Name: "worker-1"})
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	second, err := f.Provision(t.Context(), provider.Spec{Tag: "test-pool", Name: "worker-2"})
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if first.Address != "192.0.2.10" || second.Address != "192.0.2.11" {
		t.Fatalf("slot addresses = %q, %q", first.Address, second.Address)
	}
	if _, err := f.Provision(t.Context(), provider.Spec{Tag: "test-pool", Name: "worker-3"}); err == nil {
		t.Fatal("third Provision succeeded with both slots occupied")
	}

	for name, wantTap := range map[string]string{"worker-1": "tap-test", "worker-2": "tap-test-2"} {
		data, err := os.ReadFile(filepath.Join(stateDir, name, configFileName))
		if err != nil {
			t.Fatal(err)
		}
		var got vmConfig
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.NetworkInterfaces[0].HostDevice != wantTap {
			t.Errorf("%s TAP = %q, want %q", name, got.NetworkInterfaces[0].HostDevice, wantTap)
		}
	}
	for _, name := range []string{"worker-1", "worker-2"} {
		if err := f.Destroy(t.Context(), name); err != nil {
			t.Fatalf("Destroy %s: %v", name, err)
		}
	}
}

func TestProvisionStagesJailerRootAndLaunchesJailer(t *testing.T) {
	f, stateDir := configuredFirecracker(t)
	testRoot := filepath.Dir(stateDir)
	jailer := filepath.Join(testRoot, "jailer")
	firecracker := filepath.Join(testRoot, "firecracker")
	for _, path := range []string{jailer, firecracker} {
		if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	jailerBase := filepath.Join(testRoot, "jails")
	if err := os.Mkdir(jailerBase, 0o700); err != nil {
		t.Fatal(err)
	}
	f.cfg.JailerBin = jailer
	f.cfg.FirecrackerBin = firecracker
	f.cfg.JailerUID = os.Getuid()
	f.cfg.JailerGID = os.Getgid()
	f.cfg.JailerBaseDir = jailerBase
	f.cfg.JailerCgroupV = 2
	launcher := &fakeLauncher{process: &fakeProcess{pid: os.Getpid(), done: make(chan struct{})}}
	f.launcher = launcher

	instance, err := f.Provision(t.Context(), provider.Spec{Tag: "test-pool", Name: "worker-1"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if launcher.command != jailer {
		t.Fatalf("launch command = %q, want %q", launcher.command, jailer)
	}
	wantArgs := []string{
		"--id", "worker-1", "--exec-file", firecracker,
		"--uid", fmt.Sprint(os.Getuid()), "--gid", fmt.Sprint(os.Getgid()),
		"--chroot-base-dir", jailerBase, "--cgroup-version", "2",
		"--", "--no-api", "--config-file", "/firecracker.json",
	}
	if fmt.Sprint(launcher.args) != fmt.Sprint(wantArgs) {
		t.Fatalf("launch args = %v, want %v", launcher.args, wantArgs)
	}
	jailDir := filepath.Join(jailerBase, "firecracker", "worker-1")
	configData, err := os.ReadFile(filepath.Join(jailDir, "root", configFileName))
	if err != nil {
		t.Fatal(err)
	}
	var got vmConfig
	if err := json.Unmarshal(configData, &got); err != nil {
		t.Fatal(err)
	}
	if got.BootSource.KernelImage != "/kernel" || got.Drives[0].Path != "/rootfs.raw" || got.Drives[1].Path != "/seed.iso" {
		t.Fatalf("jailed paths = %+v", got)
	}
	if instance.Address != "192.0.2.10" {
		t.Fatalf("instance = %+v", instance)
	}
	if err := f.Destroy(t.Context(), "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jailDir); !os.IsNotExist(err) {
		t.Fatalf("jail remains after Destroy: %v", err)
	}
}
