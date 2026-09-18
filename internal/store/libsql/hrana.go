package libsql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file is the libsql engine's ONLY network code, and one of the
// allowlisted egress points (internal/privacy): it POSTs Hrana pipelines to
// the operator's own libsql server, and only when database.engine = "libsql".

// Pipeliner sends one Hrana pipeline. baseURL overrides the configured URL
// when a server asked the stream to continue elsewhere (the response's
// base_url); "" means the configured one. The HTTP implementation is
// httpPipeliner; tests and the store suite use hranafake.Server.
type Pipeliner interface {
	Pipeline(ctx context.Context, baseURL string, req PipelineRequest) (PipelineResponse, error)
}

// maxResponseBytes bounds one response body. The store's largest reads (a
// knowledge rebuild's embeddings) are a few MB; this is a guard against a
// misbehaving server, not a tuning knob.
const maxResponseBytes = 256 << 20

// httpPipeliner speaks Hrana over HTTP with one keep-alive client, so a warm
// statement costs a single round trip rather than a TCP+TLS handshake.
type httpPipeliner struct {
	url     string
	token   string
	timeout time.Duration
	client  *http.Client
}

func newHTTPPipeliner(url, token string, timeout time.Duration, conns int) *httpPipeliner {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = conns
	t.IdleConnTimeout = 90 * time.Second
	t.ForceAttemptHTTP2 = true
	return &httpPipeliner{url: url, token: token, timeout: timeout, client: &http.Client{Transport: t}}
}

// Pipeline implements Pipeliner. A caller context with no deadline gets the
// engine's per-request timeout: the daemon's store calls sit on its event
// loop, and a hung server must cost a bounded stall, never an unbounded one.
func (p *httpPipeliner) Pipeline(ctx context.Context, baseURL string, req PipelineRequest) (PipelineResponse, error) {
	if _, ok := ctx.Deadline(); !ok && p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("libsql: encode pipeline: %w", err)
	}
	base := p.url
	if baseURL != "" {
		base = NormalizeURL(baseURL)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/pipeline", bytes.NewReader(body))
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("libsql: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("libsql: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("libsql: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return PipelineResponse{}, statusError(resp.StatusCode, data, req.Baton != nil)
	}
	var out PipelineResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return PipelineResponse{}, fmt.Errorf("libsql: decode response: %w", err)
	}
	if len(out.Results) != len(req.Requests) {
		return PipelineResponse{}, fmt.Errorf("libsql: malformed response: %d results for %d requests",
			len(out.Results), len(req.Requests))
	}
	return out, nil
}

// statusError classifies a non-200 answer. The message always carries the
// status TEXT ("service unavailable", "bad gateway", …): the fleet recovery
// reads those shapes as a remote fault, never a reason to restart.
func statusError(code int, body []byte, hadBaton bool) error {
	detail := strings.TrimSpace(string(body))
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrUnauthorized
	case code == http.StatusNotFound && !hadBaton:
		return ErrNotHrana
	case hadBaton && code >= 400 && code < 500:
		// sqld answers a baton it no longer holds (expired, or the server
		// restarted) with a 4xx: the stream, and any transaction on it, is gone.
		return &StreamClosedError{Detail: fmt.Sprintf("HTTP %d %s: %s", code, strings.ToLower(http.StatusText(code)), detail)}
	default:
		return fmt.Errorf("libsql: HTTP %d %s: %s", code, strings.ToLower(http.StatusText(code)), detail)
	}
}

// NormalizeURL turns a configured libsql URL into the HTTP base the pipeline
// endpoint hangs off: libsql:// becomes https:// (the scheme `turso db show`
// and most providers print), http(s):// is kept, and a trailing slash is
// dropped. String handling only — net/url is outside the egress allowlist.
func NormalizeURL(raw string) string {
	u := strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(u, "libsql://"); ok {
		u = "https://" + rest
	}
	return strings.TrimRight(u, "/")
}

// ValidURL reports whether raw names an HTTP(S) libsql endpoint after
// normalization. WebSocket URLs are refused: this engine speaks Hrana over
// HTTP only.
func ValidURL(raw string) bool {
	u := NormalizeURL(raw)
	for _, p := range []string{"https://", "http://"} {
		if rest, ok := strings.CutPrefix(u, p); ok {
			return rest != ""
		}
	}
	return false
}
