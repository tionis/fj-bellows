package proxmox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

const testTag = "test-pool"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testProvider(client *http.Client) *Proxmox {
	return &Proxmox{
		cfg: config{
			Node:           "node-a",
			TemplateVMID:   9000,
			SnippetStorage: "local",
			Network:        "ciworkers",
			CIUser:         "root",
			Cores:          2,
			MemoryMB:       2048,
			TaskTimeout:    duration(time.Second),
			AddressTimeout: duration(time.Second),
			PollInterval:   duration(time.Millisecond),
		},
		tag: testTag,
		api: &apiClient{baseURL: "https://pve.example.invalid", tokenID: "id", secret: "secret", http: client},
	}
}

func dataResponse(data string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"data":%s}`, data))),
	}
}

func TestConfigureRejectsHTTP(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(`
endpoint: http://pve.example.invalid:8006
token_id: user@pve!worker
token_secret: secret
node: pve
template_vmid: 9000
snippet_storage: local
network: workers
`), &node); err != nil {
		t.Fatal(err)
	}
	p := &Proxmox{}
	if err := p.Configure(t.Context(), "pool", node); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("Configure error = %v, want HTTPS validation", err)
	}
}

func TestProvisionCloneConfigureStartAndAddress(t *testing.T) { //nolint:gocyclo // the fake API validates one complete provision workflow.
	var mu sync.Mutex
	var paths []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/cluster/nextid":
			return dataResponse(`"101"`), nil
		case strings.HasSuffix(r.URL.Path, "/upload"):
			if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // test server caps the parsed body at 1 MiB.
				t.Errorf("ParseMultipartForm: %v", err)
			}
			return dataResponse(`"local:snippets/fj-bellows-101-user.yaml"`), nil
		case strings.HasSuffix(r.URL.Path, "/clone"), strings.HasSuffix(r.URL.Path, "/status/start"):
			return dataResponse(`"UPID:node-a:test"`), nil
		case strings.Contains(r.URL.Path, "/tasks/"):
			return dataResponse(`{"status":"stopped","exitstatus":"OK"}`), nil
		case strings.HasSuffix(r.URL.Path, "/config"):
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			if r.Form.Get("tags") != testTag {
				t.Errorf("tags = %q", r.Form.Get("tags"))
			}
			if !strings.Contains(r.Form.Get("cicustom"), "fj-bellows-101-user.yaml") {
				t.Errorf("cicustom = %q", r.Form.Get("cicustom"))
			}
			return dataResponse(`null`), nil
		case strings.HasSuffix(r.URL.Path, "/agent/network-get-interfaces"):
			return dataResponse(`{"result":[{"name":"eth0","ip-addresses":[{"ip-address":"fdf0::101","ip-address-type":"ipv6"}]}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})}
	p := testProvider(client)

	inst, err := p.Provision(t.Context(), provider.Spec{
		Tag: testTag, Name: "worker-101", UserData: "#cloud-config\n",
		AuthorizedKey: "ssh-ed25519 AAAATEST",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if inst.ID != "node-a/101" || inst.Address != "fdf0::101" || inst.Tag != testTag {
		t.Errorf("instance = %+v", inst)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) < 7 {
		t.Errorf("requests = %v", paths)
	}
}

func TestListFiltersExactTag(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/cluster/resources":
			return dataResponse(`[
{"vmid":101,"node":"node-a","name":"owned","type":"qemu","status":"running","tags":"other;test-pool"},
{"vmid":102,"node":"node-a","name":"prefix","type":"qemu","status":"stopped","tags":"test-pool-extra"},
{"vmid":103,"node":"node-a","name":"container","type":"lxc","status":"stopped","tags":"` + testTag + `"}
]`), nil
		case strings.HasSuffix(r.URL.Path, "/agent/network-get-interfaces"):
			return dataResponse(`{"result":[{"name":"eth0","ip-addresses":[{"ip-address":"192.0.2.10","ip-address-type":"ipv4"}]}]}`), nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found"))}, nil
		}
	})}
	p := testProvider(client)

	instances, err := p.List(context.Background(), testTag)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(instances) != 1 || instances[0].ID != "node-a/101" || instances[0].Address != "192.0.2.10" {
		t.Fatalf("instances = %+v", instances)
	}
}

func TestDestroyVerifiesOwnershipAndDeletesSnippet(t *testing.T) {
	var deletedVM, deletedSnippet bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/qemu/101/config"):
			return dataResponse(`{"tags":"other;test-pool"}`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status/stop"):
			return dataResponse(`"UPID:node-a:stop"`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/"):
			return dataResponse(`{"status":"stopped","exitstatus":"OK"}`), nil
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/qemu/101"):
			deletedVM = true
			return dataResponse(`"UPID:node-a:delete"`), nil
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/content/"):
			deletedSnippet = true
			return dataResponse(`null`), nil
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})}
	p := testProvider(client)

	if err := p.Destroy(context.Background(), "node-a/101"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !deletedVM || !deletedSnippet {
		t.Errorf("deleted VM=%v snippet=%v", deletedVM, deletedSnippet)
	}
}

func TestDestroyRefusesForeignVM(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/qemu/101/config") {
			return dataResponse(`{"tags":"other"}`), nil
		}
		return nil, fmt.Errorf("unexpected mutation: %s %s", r.Method, r.URL.Path)
	})}
	p := testProvider(client)

	if err := p.Destroy(context.Background(), "node-a/101"); err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("Destroy error = %v, want foreign-tag refusal", err)
	}
}
