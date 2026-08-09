// Package proxmox implements provider.Provider using the Proxmox VE REST API.
package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

const (
	defaultTaskTimeout    = 3 * time.Minute
	defaultAddressTimeout = 3 * time.Minute
	defaultPollInterval   = 2 * time.Second
)

var safeTag = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

type config struct {
	Endpoint       string   `yaml:"endpoint"`
	TokenID        string   `yaml:"token_id"`
	TokenSecret    string   `yaml:"token_secret"`
	CAFile         string   `yaml:"ca_file"`
	Node           string   `yaml:"node"`
	TemplateVMID   int      `yaml:"template_vmid"`
	Storage        string   `yaml:"storage"`
	SnippetStorage string   `yaml:"snippet_storage"`
	Network        string   `yaml:"network"`
	CIUser         string   `yaml:"ci_user"`
	LinkedClone    *bool    `yaml:"linked_clone"`
	Firewall       bool     `yaml:"firewall"`
	Cores          int      `yaml:"cores"`
	MemoryMB       int      `yaml:"memory_mb"`
	DiskSize       string   `yaml:"disk_size"`
	TaskTimeout    duration `yaml:"task_timeout"`
	AddressTimeout duration `yaml:"address_timeout"`
	PollInterval   duration `yaml:"poll_interval"`
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

// Proxmox is a single-cluster Proxmox VE provider.
type Proxmox struct {
	cfg config
	tag string
	api *apiClient
}

func init() {
	provider.Register("proxmox", func() provider.Provider { return &Proxmox{} })
}

// Configure validates provider configuration and constructs the API client.
func (p *Proxmox) Configure(_ context.Context, tag string, node yaml.Node) error {
	if err := node.Decode(&p.cfg); err != nil {
		return fmt.Errorf("proxmox: decode provider_config: %w", err)
	}
	if err := p.applyDefaultsAndValidate(tag); err != nil {
		return err
	}
	httpClient, err := newHTTPClient(p.cfg.CAFile)
	if err != nil {
		return err
	}
	p.api = &apiClient{
		baseURL: strings.TrimRight(p.cfg.Endpoint, "/") + "/api2/json",
		tokenID: p.cfg.TokenID,
		secret:  p.cfg.TokenSecret,
		http:    httpClient,
	}
	p.tag = tag
	return nil
}

func (p *Proxmox) applyDefaultsAndValidate(tag string) error {
	var missing []string
	for key, value := range map[string]string{
		"endpoint": p.cfg.Endpoint, "token_id": p.cfg.TokenID,
		"token_secret": p.cfg.TokenSecret, "node": p.cfg.Node,
		"snippet_storage": p.cfg.SnippetStorage, "network": p.cfg.Network,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if p.cfg.TemplateVMID <= 0 {
		missing = append(missing, "template_vmid")
	}
	if len(missing) > 0 {
		return fmt.Errorf("proxmox: provider_config missing: %s", strings.Join(missing, ", "))
	}
	u, err := url.Parse(p.cfg.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("proxmox: endpoint must be an absolute HTTPS URL")
	}
	if !safeTag.MatchString(tag) {
		return fmt.Errorf("proxmox: tag %q contains characters unsupported by Proxmox", tag)
	}
	if p.cfg.CIUser == "" {
		p.cfg.CIUser = "root"
	}
	if p.cfg.Cores == 0 {
		p.cfg.Cores = 2
	}
	if p.cfg.MemoryMB == 0 {
		p.cfg.MemoryMB = 4096
	}
	if p.cfg.TaskTimeout == 0 {
		p.cfg.TaskTimeout = duration(defaultTaskTimeout)
	}
	if p.cfg.AddressTimeout == 0 {
		p.cfg.AddressTimeout = duration(defaultAddressTimeout)
	}
	if p.cfg.PollInterval == 0 {
		p.cfg.PollInterval = duration(defaultPollInterval)
	}
	return nil
}

func newHTTPClient(caFile string) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("proxmox: load system CA pool: %w", err)
		}
		pemBytes, err := os.ReadFile(caFile) //nolint:gosec // operator-configured CA path.
		if err != nil {
			return nil, fmt.Errorf("proxmox: read ca_file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("proxmox: ca_file contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}

// Provision clones the configured template, attaches unique cloud-init data,
// starts it, and waits for the QEMU guest agent to report a dialable address.
func (p *Proxmox) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	vmid, err := p.nextID(ctx)
	if err != nil {
		return provider.Instance{}, err
	}
	snippet := p.snippetName(vmid)
	if err := p.uploadSnippet(ctx, snippet, spec.UserData); err != nil {
		return provider.Instance{}, err
	}
	created := false
	defer func() {
		if !created {
			p.cleanupFailedProvision(vmid)
		}
	}()
	if err := p.clone(ctx, vmid, spec.Name); err != nil {
		return provider.Instance{}, err
	}
	created = true
	if err := p.configureVM(ctx, vmid, spec, snippet); err != nil {
		p.cleanupFailedProvision(vmid)
		return provider.Instance{}, err
	}
	if err := p.resizeDisk(ctx, vmid); err != nil {
		p.cleanupFailedProvision(vmid)
		return provider.Instance{}, err
	}
	if err := p.start(ctx, vmid); err != nil {
		p.cleanupFailedProvision(vmid)
		return provider.Instance{}, err
	}
	address, err := p.waitAddress(ctx, p.cfg.Node, vmid)
	if err != nil {
		p.cleanupFailedProvision(vmid)
		return provider.Instance{}, err
	}
	return provider.Instance{
		ID:        instanceID(p.cfg.Node, vmid),
		Name:      spec.Name,
		Address:   address,
		CreatedAt: time.Now().UTC(),
		Tag:       spec.Tag,
	}, nil
}

func (p *Proxmox) nextID(ctx context.Context) (int, error) {
	var raw json.RawMessage
	if err := p.api.get(ctx, "/cluster/nextid", &raw); err != nil {
		return 0, fmt.Errorf("proxmox: get next VMID: %w", err)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		id, convErr := strconv.Atoi(s)
		if convErr != nil {
			return 0, fmt.Errorf("proxmox: invalid next VMID %q: %w", s, convErr)
		}
		return id, nil
	}
	var id int
	if err := json.Unmarshal(raw, &id); err != nil {
		return 0, fmt.Errorf("proxmox: decode next VMID: %w", err)
	}
	return id, nil
}

func (p *Proxmox) uploadSnippet(ctx context.Context, name, userData string) error {
	path := fmt.Sprintf("/nodes/%s/storage/%s/upload", url.PathEscape(p.cfg.Node), url.PathEscape(p.cfg.SnippetStorage))
	var volume string
	if err := p.api.upload(ctx, path, name, []byte(userData), &volume); err != nil {
		return fmt.Errorf("proxmox: upload cloud-init snippet: %w", err)
	}
	return nil
}

func (p *Proxmox) clone(ctx context.Context, vmid int, name string) error {
	values := url.Values{
		"newid":  {strconv.Itoa(vmid)},
		"name":   {name},
		"target": {p.cfg.Node},
	}
	full := p.cfg.LinkedClone != nil && !*p.cfg.LinkedClone
	if full {
		values.Set("full", "1")
	} else {
		values.Set("full", "0")
	}
	if p.cfg.Storage != "" {
		values.Set("storage", p.cfg.Storage)
	}
	path := fmt.Sprintf("/nodes/%s/qemu/%d/clone", url.PathEscape(p.cfg.Node), p.cfg.TemplateVMID)
	return p.runTask(ctx, http.MethodPost, path, values, "clone template")
}

func (p *Proxmox) configureVM(ctx context.Context, vmid int, spec provider.Spec, snippet string) error {
	net0 := "virtio,bridge=" + p.cfg.Network
	if p.cfg.Firewall {
		net0 += ",firewall=1"
	}
	values := url.Values{
		"agent":    {"enabled=1"},
		"cicustom": {"user=" + p.cfg.SnippetStorage + ":snippets/" + snippet},
		"ciuser":   {p.cfg.CIUser},
		"cores":    {strconv.Itoa(p.cfg.Cores)},
		"memory":   {strconv.Itoa(p.cfg.MemoryMB)},
		"net0":     {net0},
		"tags":     {spec.Tag},
	}
	if spec.AuthorizedKey != "" {
		values.Set("sshkeys", strings.TrimSpace(spec.AuthorizedKey))
	}
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(p.cfg.Node), vmid)
	if err := p.api.form(ctx, http.MethodPut, path, values, nil); err != nil {
		return fmt.Errorf("proxmox: configure VM: %w", err)
	}
	return nil
}

func (p *Proxmox) resizeDisk(ctx context.Context, vmid int) error {
	if p.cfg.DiskSize == "" {
		return nil
	}
	path := fmt.Sprintf("/nodes/%s/qemu/%d/resize", url.PathEscape(p.cfg.Node), vmid)
	values := url.Values{"disk": {"scsi0"}, "size": {p.cfg.DiskSize}}
	if err := p.api.form(ctx, http.MethodPut, path, values, nil); err != nil {
		return fmt.Errorf("proxmox: resize VM disk: %w", err)
	}
	return nil
}

func (p *Proxmox) start(ctx context.Context, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/status/start", url.PathEscape(p.cfg.Node), vmid)
	return p.runTask(ctx, http.MethodPost, path, nil, "start VM")
}

func (p *Proxmox) runTask(ctx context.Context, method, path string, values url.Values, operation string) error {
	var upid string
	if err := p.api.form(ctx, method, path, values, &upid); err != nil {
		return fmt.Errorf("proxmox: %s: %w", operation, err)
	}
	if upid == "" {
		return nil
	}
	return p.waitTask(ctx, upid, operation)
}

func (p *Proxmox) waitTask(ctx context.Context, upid, operation string) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.TaskTimeout.D())
	defer cancel()
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(p.cfg.Node), url.PathEscape(upid))
	for {
		var status struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := p.api.get(ctx, path, &status); err != nil {
			return fmt.Errorf("proxmox: poll %s task: %w", operation, err)
		}
		if status.Status == "stopped" {
			if status.ExitStatus != "OK" {
				return fmt.Errorf("proxmox: %s task failed: %s", operation, status.ExitStatus)
			}
			return nil
		}
		if err := waitContext(ctx, p.cfg.PollInterval.D()); err != nil {
			return fmt.Errorf("proxmox: wait for %s task: %w", operation, err)
		}
	}
}

func (p *Proxmox) waitAddress(ctx context.Context, node string, vmid int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.AddressTimeout.D())
	defer cancel()
	for {
		address, err := p.address(ctx, node, vmid)
		if err == nil && address != "" {
			return address, nil
		}
		if waitErr := waitContext(ctx, p.cfg.PollInterval.D()); waitErr != nil {
			return "", fmt.Errorf("proxmox: wait for VM %d address: %w", vmid, waitErr)
		}
	}
}

func (p *Proxmox) address(ctx context.Context, node string, vmid int) (string, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", url.PathEscape(node), vmid)
	var response struct {
		Result []struct {
			Name        string `json:"name"`
			IPAddresses []struct {
				Address string `json:"ip-address"`
				Type    string `json:"ip-address-type"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := p.api.get(ctx, path, &response); err != nil {
		return "", err
	}
	var ipv6 string
	for _, iface := range response.Result {
		if iface.Name == "lo" {
			continue
		}
		for _, candidate := range iface.IPAddresses {
			ip := net.ParseIP(candidate.Address)
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip.To4() != nil {
				return candidate.Address, nil
			}
			if ipv6 == "" {
				ipv6 = candidate.Address
			}
		}
	}
	return ipv6, nil
}

// Destroy stops and purges a VM, then deletes its provider-owned snippet.
func (p *Proxmox) Destroy(ctx context.Context, id string) error {
	node, vmid, err := parseInstanceID(id)
	if err != nil {
		return err
	}
	tags, exists, err := p.vmTags(ctx, node, vmid)
	if err != nil {
		return err
	}
	if exists && !hasTag(tags, p.tag) {
		return fmt.Errorf("proxmox: refuse to destroy VM %s with foreign tags", id)
	}
	return p.destroyVM(ctx, node, vmid)
}

func (p *Proxmox) vmTags(ctx context.Context, node string, vmid int) (string, bool, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	var cfg struct {
		Tags string `json:"tags"`
	}
	if err := p.api.get(ctx, path, &cfg); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, fmt.Errorf("proxmox: read VM ownership: %w", err)
	}
	return cfg.Tags, true, nil
}

func (p *Proxmox) destroyVM(ctx context.Context, node string, vmid int) error {
	stopPath := fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", url.PathEscape(node), vmid)
	_ = p.runTask(ctx, http.MethodPost, stopPath, url.Values{"skiplock": {"0"}}, "stop VM")
	deletePath := fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(node), vmid)
	values := url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}
	if err := p.runTask(ctx, http.MethodDelete, deletePath, values, "destroy VM"); err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
			return err
		}
	}
	if err := p.deleteSnippet(ctx, node, vmid); err != nil {
		return err
	}
	return nil
}

func (p *Proxmox) deleteSnippet(ctx context.Context, node string, vmid int) error {
	volume := p.cfg.SnippetStorage + ":snippets/" + p.snippetName(vmid)
	path := fmt.Sprintf(
		"/nodes/%s/storage/%s/content/%s",
		url.PathEscape(node), url.PathEscape(p.cfg.SnippetStorage), url.PathEscape(volume),
	)
	if err := p.api.form(ctx, http.MethodDelete, path, nil, nil); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("proxmox: delete cloud-init snippet: %w", err)
	}
	return nil
}

// List returns QEMU VMs carrying the exact deployment tag.
func (p *Proxmox) List(ctx context.Context, tag string) ([]provider.Instance, error) {
	var resources []struct {
		VMID   int    `json:"vmid"`
		Node   string `json:"node"`
		Name   string `json:"name"`
		Type   string `json:"type"`
		Status string `json:"status"`
		Tags   string `json:"tags"`
	}
	if err := p.api.get(ctx, "/cluster/resources?type=vm", &resources); err != nil {
		return nil, fmt.Errorf("proxmox: list VMs: %w", err)
	}
	instances := make([]provider.Instance, 0)
	for _, resource := range resources {
		if resource.Type != "qemu" || !hasTag(resource.Tags, tag) {
			continue
		}
		address := ""
		if resource.Status == "running" {
			address, _ = p.address(ctx, resource.Node, resource.VMID)
		}
		instances = append(instances, provider.Instance{
			ID:      instanceID(resource.Node, resource.VMID),
			Name:    resource.Name,
			Address: address,
			Tag:     tag,
		})
	}
	return instances, nil
}

// BillingModel reports local capacity with no external billing boundary.
func (p *Proxmox) BillingModel() provider.BillingModel { return provider.BillingPerSecond }

func (p *Proxmox) cleanupFailedProvision(vmid int) {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.TaskTimeout.D())
	defer cancel()
	_ = p.destroyVM(ctx, p.cfg.Node, vmid)
}

func (p *Proxmox) snippetName(vmid int) string {
	return fmt.Sprintf("fj-bellows-%d-user.yaml", vmid)
}

func instanceID(node string, vmid int) string { return node + "/" + strconv.Itoa(vmid) }

func parseInstanceID(id string) (string, int, error) {
	node, raw, ok := strings.Cut(id, "/")
	if !ok || node == "" {
		return "", 0, fmt.Errorf("proxmox: invalid instance ID %q", id)
	}
	vmid, err := strconv.Atoi(raw)
	if err != nil || vmid <= 0 {
		return "", 0, fmt.Errorf("proxmox: invalid instance ID %q", id)
	}
	return node, vmid, nil
}

func hasTag(tags, wanted string) bool {
	for tag := range strings.SplitSeq(tags, ";") {
		if tag == wanted {
			return true
		}
	}
	return false
}

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
