package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

const capacityErrorBody = `{"code":null,"message":"The model is currently at capacity due to high demand. Please try again in a few minutes, or use a higher service tier for priority processing: https://docs.x.ai/developers/advanced-api-usage/priority-processing","param":null,"type":"error"}`

func TestIsSoftCapacityErrorFlatJSON(t *testing.T) {
	if !isSoftCapacityError([]byte(capacityErrorBody)) {
		t.Fatal("flat capacity JSON must match")
	}
}

func TestIsSoftCapacityErrorNestedJSON(t *testing.T) {
	body := `{"error":{"message":"The model is currently at capacity due to high demand.","type":"error","code":null}}`
	if !isSoftCapacityError([]byte(body)) {
		t.Fatal("nested capacity JSON must match")
	}
}

func TestIsSoftCapacityErrorSSE(t *testing.T) {
	body := "data: " + capacityErrorBody + "\n\n"
	if !isSoftCapacityError([]byte(body)) {
		t.Fatal("SSE capacity error event must match")
	}
}

func TestIsSoftCapacityErrorRejectsBusinessError(t *testing.T) {
	body := `{"type":"error","message":"invalid request: messages is required","code":"invalid_request"}`
	if isSoftCapacityError([]byte(body)) {
		t.Fatal("ordinary business error must not match capacity soft-error")
	}
}

func TestIsSoftCapacityErrorRejectsHealthyStream(t *testing.T) {
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"
	if isSoftCapacityError([]byte(body)) {
		t.Fatal("healthy stream first event must not match")
	}
}

func TestRewriteSoftCapacityResponseRewritesStatusAndPreservesBody(t *testing.T) {
	response := &provider.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(capacityErrorBody)),
	}
	if !rewriteSoftCapacityResponse(response) {
		t.Fatal("expected rewrite")
	}
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", response.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("at capacity")) {
		t.Fatalf("body lost capacity message: %s", body)
	}
}

func TestRewriteSoftCapacityResponsePreservesHealthyStreamPrefix(t *testing.T) {
	payload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\ndata: {\"type\":\"response.completed\"}\n\n"
	response := &provider.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}
	if rewriteSoftCapacityResponse(response) {
		t.Fatal("healthy stream must not be rewritten")
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != payload {
		t.Fatalf("prefix restore lost bytes:\n got %q\nwant %q", body, payload)
	}
}

func TestRewriteSoftCapacityResponseLeavesNon2xxAlone(t *testing.T) {
	response := &provider.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(capacityErrorBody)),
	}
	if rewriteSoftCapacityResponse(response) {
		t.Fatal("non-2xx must not be rewritten")
	}
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestApplySoftCapacityFailureClearsAccountPenalties(t *testing.T) {
	failure := newHTTPUpstreamFailure(http.StatusTooManyRequests, []byte(capacityErrorBody), 7, "acct")
	if !failure.AccountScoped {
		t.Fatal("raw 429 classification should be account-scoped before soft-capacity override")
	}
	applySoftCapacityFailure(failure)
	if failure.AccountScoped || failure.QuotaExhausted || failure.FreeQuotaExhausted || failure.ModelQuotaExhausted {
		t.Fatalf("soft capacity must not mark account/quota failure: %#v", failure)
	}
	if failure.Code != "upstream_model_capacity" || failure.Fingerprint != softCapacityFingerprint {
		t.Fatalf("failure = %#v", failure)
	}
	if failure.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("status = %d", failure.HTTPStatus)
	}
}

func TestPrefixReadCloserSurfacesEOFAfterPrefix(t *testing.T) {
	reader := &prefixReadCloser{prefix: []byte("hello"), rest: nil, readErr: io.EOF}
	buf := make([]byte, 16)
	n, err := reader.Read(buf)
	if n != 5 || string(buf[:n]) != "hello" {
		t.Fatalf("first read n=%d data=%q err=%v", n, buf[:n], err)
	}
	// After prefix is exhausted, next read should surface EOF.
	n, err = reader.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("second read n=%d err=%v", n, err)
	}
}

func TestGatewayRotatesOnHTTP200SoftCapacityWithoutMarkFailure(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "soft-capacity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	first, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "capacity-first", SourceKey: "capacity-first", EncryptedAccessToken: "one", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "capacity-second", SourceKey: "capacity-second", EncryptedAccessToken: "two", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-capacity"}); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []uint64{first.ID, second.ID} {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, accountID, []string{"grok-capacity"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{Name: "capacity-key", Prefix: "cap-prefix", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-key", Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}

	adapter := &failoverAdapter{
		firstID: first.ID, failureStatus: http.StatusOK, failureBody: capacityErrorBody,
	}
	registry := provider.NewRegistry(adapter)
	cipher := testCipher(t)
	sticky := memory.NewStickyStore()
	concurrency := memory.NewConcurrencyLimiter()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	clientService := clientkeyapp.NewService(nil, nil, nil, 60, 4, nil)
	selector := NewSelector(accountRepo, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientService, registry, selector, responseRepo, 3)

	// Non-stream: 200 capacity on first account must rotate to second without cooling the first.
	result, err := service.CreateResponse(ctx, Input{RequestID: "req-capacity", ClientKey: clientKey, PublicModel: "grok-capacity", Body: []byte(`{"model":"grok-capacity"}`)})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "resp-capacity", "")
	_ = result.Body.Close()
	if result.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("status=%d body=%q", result.StatusCode, body)
	}
	if len(adapter.attempts) != 2 || adapter.attempts[0] != first.ID || adapter.attempts[1] != second.ID {
		t.Fatalf("attempts = %#v", adapter.attempts)
	}
	firstAfter, err := accountRepo.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstAfter.FailureCount != 0 || firstAfter.CooldownUntil != nil {
		t.Fatalf("soft capacity must not MarkFailure first account: FailureCount=%d CooldownUntil=%v", firstAfter.FailureCount, firstAfter.CooldownUntil)
	}

	// Stream request with the same 200 capacity JSON body must also rotate before headers are committed.
	adapter.resetAttempts()
	streamed, err := service.CreateResponse(ctx, Input{
		RequestID: "req-capacity-stream", ClientKey: clientKey, PublicModel: "grok-capacity",
		Body: []byte(`{"model":"grok-capacity","stream":true}`), Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(streamed.Body)
	streamed.Finalize(Usage{}, "resp-capacity-stream", "")
	_ = streamed.Body.Close()
	if len(adapter.attempts) != 2 || adapter.attempts[0] != first.ID || adapter.attempts[1] != second.ID {
		t.Fatalf("stream attempts = %#v", adapter.attempts)
	}
	firstAfterStream, err := accountRepo.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstAfterStream.FailureCount != 0 || firstAfterStream.CooldownUntil != nil {
		t.Fatalf("stream soft capacity must not MarkFailure: FailureCount=%d CooldownUntil=%v", firstAfterStream.FailureCount, firstAfterStream.CooldownUntil)
	}
}
