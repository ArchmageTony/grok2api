package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// softCapacityPeekBytes limits how far the gateway looks into a 2xx body before deciding
// whether an upstream capacity soft-error should be rewritten to a retryable 429.
// Capacity error payloads are small; keeping the peek modest avoids delaying healthy streams.
const softCapacityPeekBytes = 8 << 10

const softCapacityFingerprint = "429:model_capacity"

// rewriteSoftCapacityResponse rewrites a 2xx response whose body is an xAI model-capacity
// error into HTTP 429 so the existing account-rotation path can run before any client
// response headers are written. Stream and non-stream share this path.
//
// Fail-open: peek/read errors leave the original response intact for the success path.
func rewriteSoftCapacityResponse(response *provider.Response) bool {
	if response == nil || response.Body == nil {
		return false
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false
	}

	buf := make([]byte, softCapacityPeekBytes)
	n, readErr := response.Body.Read(buf)
	prefix := buf[:n]
	if n == 0 {
		// Nothing buffered; restore any terminal error onto an empty body and leave status alone.
		response.Body = &prefixReadCloser{rest: response.Body, readErr: readErr}
		return false
	}
	if !isSoftCapacityError(prefix) {
		response.Body = &prefixReadCloser{prefix: prefix, rest: response.Body, readErr: readErr}
		return false
	}

	full := append([]byte(nil), prefix...)
	if readErr == nil {
		rest, restErr := io.ReadAll(io.LimitReader(response.Body, provider.MaxDiagnosticBodyBytes))
		if len(rest) > 0 {
			full = append(full, rest...)
		}
		if restErr != nil && restErr != io.EOF {
			// Body is already a capacity error; keep what we have and still rewrite.
			_ = restErr
		}
	}
	_ = response.Body.Close()

	response.StatusCode = http.StatusTooManyRequests
	response.Status = http.StatusText(http.StatusTooManyRequests)
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	// Capacity soft-errors are JSON (sometimes delivered on a stream request). Prefer JSON
	// so downstream clients and new-api treat the final failure as a normal error body.
	response.Header.Set("Content-Type", "application/json")
	response.Body = io.NopCloser(bytes.NewReader(full))
	return true
}

// isSoftCapacityError reports whether body is an xAI model-capacity soft-error.
// Requires an error-shaped payload plus capacity wording so ordinary business errors are not retried.
func isSoftCapacityError(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return false
	}
	if matchSoftCapacityJSON(body) {
		return true
	}
	// Stream requests occasionally receive a single SSE error event with HTTP 200.
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if matchSoftCapacityJSON(payload) {
			return true
		}
	}
	return false
}

func matchSoftCapacityJSON(data []byte) bool {
	code, errorType, message := extractUpstreamErrorMetadata(data)
	if !hasSoftCapacitySignal(code, errorType, message) {
		return false
	}
	// Prefer explicit error typing; also accept capacity wording with a nested/flat error shape
	// even when type is empty, as long as extractUpstreamErrorMetadata found a message.
	typeLower := strings.ToLower(strings.TrimSpace(errorType))
	if typeLower == "error" || typeLower == "server_error" || typeLower == "overloaded_error" {
		return true
	}
	if message != "" && (typeLower == "" || strings.Contains(typeLower, "error")) {
		return true
	}
	return false
}

func hasSoftCapacitySignal(parts ...string) bool {
	text := strings.ToLower(strings.Join(parts, " "))
	return strings.Contains(text, "at capacity") ||
		strings.Contains(text, "priority processing") ||
		strings.Contains(text, "higher service tier") ||
		strings.Contains(text, "currently at capacity")
}

// applySoftCapacityFailure shapes a rewritten capacity failure for account rotation:
// retryable 429, no account cooldown/MarkFailure, stable fingerprint for diagnostics.
func applySoftCapacityFailure(failure *UpstreamFailure) {
	if failure == nil {
		return
	}
	failure.HTTPStatus = http.StatusTooManyRequests
	failure.Code = "upstream_model_capacity"
	failure.PublicMessage = "上游模型容量不足"
	failure.AccountScoped = false
	failure.AccountBlocked = false
	failure.PermanentAccountDenial = false
	failure.QuotaExhausted = false
	failure.FreeQuotaExhausted = false
	failure.ModelQuotaExhausted = false
	failure.CredentialRejected = false
	failure.Fingerprint = softCapacityFingerprint
}

type prefixReadCloser struct {
	prefix  []byte
	off     int
	rest    io.ReadCloser
	readErr error
}

func (p *prefixReadCloser) Read(dst []byte) (int, error) {
	if p.off < len(p.prefix) {
		n := copy(dst, p.prefix[p.off:])
		p.off += n
		if p.off < len(p.prefix) {
			return n, nil
		}
		// Prefix exhausted. If the original read already ended, surface that after the prefix.
		if p.rest == nil {
			if p.readErr != nil {
				return n, p.readErr
			}
			return n, io.EOF
		}
		if n > 0 {
			return n, nil
		}
	}
	if p.rest == nil {
		if p.readErr != nil {
			return 0, p.readErr
		}
		return 0, io.EOF
	}
	n, err := p.rest.Read(dst)
	if n == 0 && err == nil && p.readErr != nil {
		return 0, p.readErr
	}
	if err == io.EOF && p.readErr != nil && p.readErr != io.EOF {
		return n, p.readErr
	}
	return n, err
}

func (p *prefixReadCloser) Close() error {
	if p.rest != nil {
		return p.rest.Close()
	}
	return nil
}
