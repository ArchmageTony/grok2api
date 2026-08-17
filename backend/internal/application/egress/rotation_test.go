package egress

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type rotationFakeRepository struct {
	node domain.Node
	err  error
}

func (r *rotationFakeRepository) ListEgressNodes(context.Context, domain.Scope, repository.SortQuery) ([]domain.Node, error) {
	return []domain.Node{r.node}, nil
}
func (r *rotationFakeRepository) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	if r.err != nil {
		return domain.Node{}, r.err
	}
	return r.node, nil
}
func (r *rotationFakeRepository) CreateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	return value, nil
}
func (r *rotationFakeRepository) UpdateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	return value, nil
}
func (r *rotationFakeRepository) DeleteEgressNode(context.Context, uint64) error { return nil }
func (r *rotationFakeRepository) ListEgressNodePage(context.Context, repository.EgressNodeListQuery) ([]domain.Node, int64, error) {
	return []domain.Node{r.node}, 1, nil
}

type rotationFakeAccounts struct {
	credentials []accountdomain.Credential
}

func (a *rotationFakeAccounts) CountProviderAccountsByIDs(context.Context, accountdomain.Provider, []uint64) (int64, error) {
	return int64(len(a.credentials)), nil
}
func (a *rotationFakeAccounts) UpdateEgressBindings(context.Context, accountdomain.Provider, []uint64, *uint64, accountdomain.EgressAssignmentMode, time.Time) (int64, error) {
	return 0, nil
}
func (a *rotationFakeAccounts) ListEgressAssignments(context.Context, accountdomain.Provider) ([]accountdomain.Credential, error) {
	return a.credentials, nil
}
func (a *rotationFakeAccounts) ListEgressBindingProviders(context.Context, uint64) ([]accountdomain.Provider, error) {
	return nil, nil
}
func (a *rotationFakeAccounts) ListEgressSourceBindingProviders(context.Context, uint64) ([]accountdomain.Provider, error) {
	return nil, nil
}

type resinCallRecorder struct {
	mu      sync.Mutex
	deleted []string
}

func newResinTestServer(recorder *resinCallRecorder) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/platforms":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"items": []map[string]string{{"id": "331ca190-1696-4cab-9982-cddb6793b9ce", "name": "US"}},
			})
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/api/v1/platforms/"):
			recorder.mu.Lock()
			recorder.deleted = append(recorder.deleted, request.URL.Path)
			recorder.mu.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newRotationService(t *testing.T, proxyTemplate string, credentials []accountdomain.Credential, recorder *resinCallRecorder, echoIPs []string) (*Service, *[]string) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt(proxyTemplate)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(
		&rotationFakeRepository{node: domain.Node{ID: 7, Scope: domain.ScopeBuild, EncryptedProxyURL: encrypted, ExitIP: "198.51.100.1"}},
		cipher, "test-agent", &rotationFakeAccounts{credentials: credentials},
	)
	return service, &echoIPs
}

func TestRotateEgressLeaseRejectsDirectProxyNode(t *testing.T) {
	recorder := &resinCallRecorder{}
	server := newResinTestServer(recorder)
	defer server.Close()
	service, _ := newRotationService(t, "http://203.0.113.9:8080", nil, recorder, nil)
	service.SetResinRotation(ResinRotationConfig{BaseURL: server.URL, AdminToken: "admin", Timeout: 10 * time.Second})

	result, err := service.RotateEgressLease(context.Background(), 7, "198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.Reason != "not_resin_backed" {
		t.Fatalf("direct node result = %+v", result)
	}
	if len(recorder.deleted) != 0 {
		t.Fatalf("direct node must not touch resin: %v", recorder.deleted)
	}
}

func TestRotateEgressLeaseReleasesBoundAccountLeases(t *testing.T) {
	recorder := &resinCallRecorder{}
	server := newResinTestServer(recorder)
	defer server.Close()
	proxyTemplate := "socks5h://US.%7Baccount%7D:token@" + strings.TrimPrefix(server.URL, "http://")
	credentials := []accountdomain.Credential{
		{ID: 6444, Provider: accountdomain.ProviderBuild, Enabled: true, EgressNodeID: 7},
		{ID: 9, Provider: accountdomain.ProviderBuild, Enabled: true, EgressNodeID: 7, EgressIdentity: "sso_abc"},
		{ID: 10, Provider: accountdomain.ProviderBuild, Enabled: true, EgressNodeID: 8, EgressIdentity: "sso_other"},
		{ID: 11, Provider: accountdomain.ProviderBuild, Enabled: false, EgressNodeID: 7, EgressIdentity: "sso_disabled"},
	}
	service, echoIPs := newRotationService(t, proxyTemplate, credentials, recorder, []string{"192.0.2.10"})
	service.SetResinRotation(ResinRotationConfig{BaseURL: server.URL, AdminToken: "admin", Timeout: 10 * time.Second})
	service.mu.Lock()
	index := 0
	service.rotation.echo = func(context.Context, string) (string, error) {
		value := (*echoIPs)[index]
		index++
		return value, nil
	}
	service.mu.Unlock()

	result, err := service.RotateEgressLease(context.Background(), 7, "198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.ExitIP != "192.0.2.10" {
		t.Fatalf("rotation result = %+v", result)
	}
	expected := []string{
		"/api/v1/platforms/331ca190-1696-4cab-9982-cddb6793b9ce/leases/grok_build_6444",
		"/api/v1/platforms/331ca190-1696-4cab-9982-cddb6793b9ce/leases/sso_abc",
	}
	if strings.Join(recorder.deleted, ",") != strings.Join(expected, ",") {
		t.Fatalf("deleted leases = %v, want %v", recorder.deleted, expected)
	}
}

func TestRotateEgressLeaseReportsUnchangedAfterRetry(t *testing.T) {
	recorder := &resinCallRecorder{}
	server := newResinTestServer(recorder)
	defer server.Close()
	proxyTemplate := "socks5h://US.%7Baccount%7D:token@" + strings.TrimPrefix(server.URL, "http://")
	credentials := []accountdomain.Credential{
		{ID: 6444, Provider: accountdomain.ProviderBuild, Enabled: true, EgressNodeID: 7},
	}
	service, _ := newRotationService(t, proxyTemplate, credentials, recorder, nil)
	service.SetResinRotation(ResinRotationConfig{BaseURL: server.URL, AdminToken: "admin", Timeout: 10 * time.Second})
	service.mu.Lock()
	service.rotation.echo = func(context.Context, string) (string, error) { return "198.51.100.1", nil }
	service.mu.Unlock()

	result, err := service.RotateEgressLease(context.Background(), 7, "198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.Reason != "exit_ip_unchanged" {
		t.Fatalf("unchanged result = %+v", result)
	}
	if len(recorder.deleted) != 2 {
		t.Fatalf("expected one retry, deleted = %v", recorder.deleted)
	}
}

func TestRotateEgressLeaseWithoutBoundAccounts(t *testing.T) {
	recorder := &resinCallRecorder{}
	server := newResinTestServer(recorder)
	defer server.Close()
	proxyTemplate := "socks5h://US.%7Baccount%7D:token@" + strings.TrimPrefix(server.URL, "http://")
	service, _ := newRotationService(t, proxyTemplate, nil, recorder, nil)
	service.SetResinRotation(ResinRotationConfig{BaseURL: server.URL, AdminToken: "admin", Timeout: 10 * time.Second})

	result, err := service.RotateEgressLease(context.Background(), 7, "198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.Reason != "no_bound_accounts" {
		t.Fatalf("unbound result = %+v", result)
	}
}

func TestRotateEgressLeaseRequiresConfiguration(t *testing.T) {
	service, _ := newRotationService(t, "http://203.0.113.9:8080", nil, &resinCallRecorder{}, nil)
	if _, err := service.RotateEgressLease(context.Background(), 7, ""); !strings.Contains(err.Error(), "未配置") {
		t.Fatalf("unconfigured rotation error = %v", err)
	}
}

func TestParseEchoIPFormats(t *testing.T) {
	if ip := parseEchoIP([]byte("fl=123\nh=1.1.1.1\nip=203.0.113.7\nts=1\n")); ip != "203.0.113.7" {
		t.Fatalf("trace parse = %q", ip)
	}
	if ip := parseEchoIP([]byte(`{"ip":"2001:db8::1"}`)); ip != "2001:db8::1" {
		t.Fatalf("json parse = %q", ip)
	}
	if ip := parseEchoIP([]byte("garbage")); ip != "" {
		t.Fatalf("garbage parse = %q", ip)
	}
}
