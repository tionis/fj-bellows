package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	// Reject anything that could escape the per-test temp dir. All
	// callers pass test-supplied literals (e.g. "config.yaml"); this
	// guard clears the gosec G703 taint without hand-waving it via
	// //nolint, and protects against a future caller getting clever.
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		t.Fatalf("writeTemp: unsafe name %q", name)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndDeferredDecode(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo:
  url: https://forgejo.example.com
  token: secret-token
  scope: orgs/example
  labels: [ubuntu-latest]
provider: linode
provider_config:
  region: example-region
  type: example-type
ssh:
  private_key_file: /tmp/id
worker:
  swap_mb: 4096
  prepared_image: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tag != "fj-bellows" {
		t.Errorf("default tag = %q", cfg.Tag)
	}
	if cfg.Scale.Max != 1 {
		t.Errorf("default max = %d", cfg.Scale.Max)
	}
	if cfg.WorkerLifecycle != WorkerLifecycleReusable {
		t.Errorf("default worker_lifecycle = %q", cfg.WorkerLifecycle)
	}
	if cfg.Poll.Interval.D() != 10*time.Second {
		t.Errorf("default interval = %s", cfg.Poll.Interval.D())
	}
	if cfg.Poll.BillingHour.D() != time.Hour {
		t.Errorf("default billing_hour = %s, want 1h", cfg.Poll.BillingHour.D())
	}
	if cfg.SSH.User != "root" || cfg.SSH.Port != 22 {
		t.Errorf("ssh defaults = %q:%d", cfg.SSH.User, cfg.SSH.Port)
	}
	if cfg.Worker.SwapMB != 4096 {
		t.Errorf("worker swap = %d MiB", cfg.Worker.SwapMB)
	}
	if !cfg.Worker.PreparedImage {
		t.Error("worker prepared_image = false, want true")
	}

	// provider_config must survive as a decodable node, opaque to core.
	var pc struct {
		Region string `yaml:"region"`
		Type   string `yaml:"type"`
	}
	if err := cfg.ProviderConfig.Decode(&pc); err != nil {
		t.Fatalf("decode provider_config: %v", err)
	}
	if pc.Region != "example-region" || pc.Type != "example-type" {
		t.Errorf("provider_config = %+v", pc)
	}
}

func TestLoadRejectsNegativeWorkerSwap(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: orgs/x}
provider: linode
worker: {swap_mb: -1}
ssh: {private_key_file: k}
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "worker.swap_mb") {
		t.Fatalf("expected worker.swap_mb validation error, got %v", err)
	}
}

func TestLoadPrewarmDisposable(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: orgs/x}
provider: linode
worker_lifecycle: disposable
scale: {max: 2, prewarm: 2}
ssh: {private_key_file: k}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scale.Prewarm != 2 {
		t.Fatalf("scale.prewarm = %d, want 2", cfg.Scale.Prewarm)
	}
}

func TestLoadRejectsPrewarmForReusableWorkers(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: orgs/x}
provider: linode
scale: {max: 1, prewarm: 1}
ssh: {private_key_file: k}
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "requires worker_lifecycle") {
		t.Fatalf("expected prewarm lifecycle validation error, got %v", err)
	}
}

func TestLoadRejectsPrewarmAboveMax(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: orgs/x}
provider: linode
worker_lifecycle: disposable
scale: {max: 1, prewarm: 2}
ssh: {private_key_file: k}
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "scale.prewarm") {
		t.Fatalf("expected prewarm range validation error, got %v", err)
	}
}

func TestLoadDisposableWorkerLifecycle(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: orgs/x}
provider: linode
worker_lifecycle: disposable
ssh: {private_key_file: k}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerLifecycle != WorkerLifecycleDisposable {
		t.Errorf("worker_lifecycle = %q", cfg.WorkerLifecycle)
	}
}

func TestLoadRejectsUnknownWorkerLifecycle(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: orgs/x}
provider: linode
worker_lifecycle: sometimes
ssh: {private_key_file: k}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid worker_lifecycle error")
	}
}

func TestLoadDurations(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: orgs/x}
provider: linode
ssh: {private_key_file: k}
poll:
  interval: 30s
  idle_timeout: 2m
  hour_margin: 90s
  billing_hour: 5m
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Poll.Interval.D() != 30*time.Second {
		t.Errorf("interval = %s", cfg.Poll.Interval.D())
	}
	if cfg.Poll.IdleTimeout.D() != 2*time.Minute {
		t.Errorf("idle = %s", cfg.Poll.IdleTimeout.D())
	}
	if cfg.Poll.HourMargin.D() != 90*time.Second {
		t.Errorf("margin = %s", cfg.Poll.HourMargin.D())
	}
	if cfg.Poll.BillingHour.D() != 5*time.Minute {
		t.Errorf("billing_hour = %s", cfg.Poll.BillingHour.D())
	}
}

func TestLoadMissingRequired(t *testing.T) {
	path := writeTemp(t, "config.yaml", `provider: linode`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadDockerProviderSkipsSSHKey(t *testing.T) {
	// The docker provider dispatches via `docker exec` and needs no SSH key.
	path := writeTemp(t, "config.yaml", `
forgejo:
  url: https://forgejo.example.com
  token: secret-token
  scope: orgs/example
  labels: [docker]
provider: docker
provider_config:
  image: example/worker:latest
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadNonDockerProviderRequiresSSHKey(t *testing.T) {
	// A non-docker provider without an SSH key is still rejected.
	path := writeTemp(t, "config.yaml", `
forgejo:
  url: https://forgejo.example.com
  token: secret-token
  scope: orgs/example
provider: linode
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error: ssh.private_key_file missing for non-docker provider")
	}
}

func TestLoadBadDuration(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: s}
provider: linode
ssh: {private_key_file: k}
poll: {interval: not-a-duration}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected duration parse error")
	}
}
