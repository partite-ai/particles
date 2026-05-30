package runtime

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
)

// TraceLevel controls how much of each request/response a
// [TracingHTTPDoer] writes to its sink.
type TraceLevel int

const (
	// TraceOff disables tracing. A WithHTTPTrace option built
	// with TraceOff is a no-op — the inner doer is returned
	// unwrapped.
	TraceOff TraceLevel = iota
	// TraceBasic logs one line per request: direction marker,
	// method, URL, status, and wall-clock duration.
	TraceBasic
	// TraceHeaders adds request and response headers, with
	// known credential-bearing headers redacted.
	TraceHeaders
	// TraceFull adds request and response bodies, truncated to
	// [traceBodyLimit] bytes. Bodies are buffered into memory
	// for the trace before being forwarded, so this level is
	// not appropriate for very large payloads. Bodies carrying
	// a recognized Content-Encoding (gzip, deflate, br) are
	// decoded for display and prefixed with a size summary —
	// the bytes forwarded to the caller remain on-wire encoded
	// so downstream code keeps owning Content-Encoding.
	TraceFull
)

// ParseTraceLevel resolves the human-facing strings
// "basic"/"headers"/"full" (case-insensitive) used by the
// `--trace-http` CLI flag.
func ParseTraceLevel(s string) (TraceLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "basic":
		return TraceBasic, nil
	case "headers":
		return TraceHeaders, nil
	case "full":
		return TraceFull, nil
	}
	return TraceOff, fmt.Errorf("invalid trace level %q (want one of: basic, headers, full)", s)
}

// traceBodyLimit caps how many bytes of a request or response
// body the tracer copies into the log. Picked large enough for
// typical JSON API payloads, small enough to keep a noisy run
// from flooding stderr.
const traceBodyLimit = 4096

// sensitiveHeaders is the set of header names whose value the
// tracer replaces with "<redacted>" at TraceHeaders and above.
// The match is case-insensitive (net/http canonicalizes header
// keys on Get/Set; we canonicalize here as well to handle the
// raw map iteration).
var sensitiveHeaders = map[string]struct{}{
	http.CanonicalHeaderKey("Authorization"):       {},
	http.CanonicalHeaderKey("Proxy-Authorization"): {},
	http.CanonicalHeaderKey("Cookie"):              {},
	http.CanonicalHeaderKey("Set-Cookie"):          {},
}

// sensitiveQueryParams are query-string keys whose value the
// tracer redacts when echoing the URL. This is a defense in
// depth — the credentials package puts API keys in headers or
// query params at well-known names, and a particle author may
// also put a token in a query param by hand. Match is
// case-insensitive on the parameter key.
var sensitiveQueryParams = map[string]struct{}{
	"api_key":      {},
	"apikey":       {},
	"key":          {},
	"token":        {},
	"access_token": {},
	"secret":       {},
}

// TracingHTTPDoer wraps an [HTTPDoer], writing a record of every
// request/response pair to W at the configured Level. It is the
// concrete type [WithHTTPTrace] installs.
//
// The wrapper sits inside the per-particle wasi:http policy, so
// every request it sees has already been validated against the
// allowed-hosts list and had its credential placeholders
// substituted for real secret values. The tracer therefore
// redacts well-known secret-bearing headers and query
// parameters before writing.
//
// Writes are serialized by an internal mutex so concurrent
// requests don't interleave their lines on the sink.
type TracingHTTPDoer struct {
	Inner HTTPDoer
	W     io.Writer
	Level TraceLevel
	// Now is the clock used to measure request duration. nil
	// → time.Now. Override in tests for deterministic timing.
	Now func() time.Time

	mu sync.Mutex
}

// Do implements [HTTPDoer].
func (t *TracingHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	now := t.Now
	if now == nil {
		now = time.Now
	}

	var (
		reqBody    []byte
		reqDisplay []byte
		reqLabel   string
	)
	if t.Level >= TraceFull && req.Body != nil {
		// Buffer the request body so we can both log it and
		// forward it. GetBody is the canonical way to obtain
		// a fresh reader for retries; if it's set we copy
		// from it. Otherwise we drain and replace.
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			t.writeLine(fmt.Sprintf("--> %s %s  (read body: %v)", req.Method, redactedURL(req.URL.String()), err))
			return nil, err
		}
		reqBody = b
		req.Body = io.NopCloser(bytes.NewReader(b))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
		reqDisplay, reqLabel = decodeBodyForTrace(reqBody, req.Header.Get("Content-Encoding"))
	}

	t.logRequest(req, reqDisplay, reqLabel)

	start := now()
	resp, err := t.Inner.Do(req)
	dur := now().Sub(start)

	if err != nil {
		t.writeLine(fmt.Sprintf("<-- %s %s  ERROR  %s  (%s)",
			req.Method, redactedURL(req.URL.String()), formatDuration(dur), err))
		return nil, err
	}

	var (
		respDisplay []byte
		respLabel   string
	)
	if t.Level >= TraceFull && resp.Body != nil {
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.writeLine(fmt.Sprintf("<-- %s %s  %d  %s  (read body: %v)",
				req.Method, redactedURL(req.URL.String()), resp.StatusCode, formatDuration(dur), err))
			return nil, err
		}
		// Hand the caller back the original wire bytes —
		// the tracer must be transparent. Decoding is for
		// display only; downstream code (gaxios, fetch, …)
		// owns Content-Encoding handling.
		resp.Body = io.NopCloser(bytes.NewReader(b))
		respDisplay, respLabel = decodeBodyForTrace(b, resp.Header.Get("Content-Encoding"))
	}

	t.logResponse(req, resp, dur, respDisplay, respLabel)
	return resp, nil
}

func (t *TracingHTTPDoer) logRequest(req *http.Request, body []byte, encodingLabel string) {
	var b strings.Builder
	fmt.Fprintf(&b, "--> %s %s", req.Method, redactedURL(req.URL.String()))
	if t.Level >= TraceHeaders {
		b.WriteString("\n")
		writeHeaders(&b, "    ", req.Header)
	}
	if t.Level >= TraceFull && len(body) > 0 {
		b.WriteString("\n")
		writeBody(&b, "    ", body, encodingLabel)
	}
	t.writeLine(b.String())
}

func (t *TracingHTTPDoer) logResponse(req *http.Request, resp *http.Response, dur time.Duration, body []byte, encodingLabel string) {
	var b strings.Builder
	fmt.Fprintf(&b, "<-- %s %s  %d  %s",
		req.Method, redactedURL(req.URL.String()), resp.StatusCode, formatDuration(dur))
	if t.Level >= TraceHeaders {
		b.WriteString("\n")
		writeHeaders(&b, "    ", resp.Header)
	}
	if t.Level >= TraceFull && len(body) > 0 {
		b.WriteString("\n")
		writeBody(&b, "    ", body, encodingLabel)
	}
	t.writeLine(b.String())
}

func (t *TracingHTTPDoer) writeLine(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = io.WriteString(t.W, s)
	if !strings.HasSuffix(s, "\n") {
		_, _ = io.WriteString(t.W, "\n")
	}
}

// writeHeaders renders h to b, one canonical-cased line per
// header (indented by prefix), with sensitive header values
// replaced by "<redacted>".
func writeHeaders(b *strings.Builder, prefix string, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	// Sort for stable output (matters for tests and for diffing
	// trace runs against each other).
	sortStrings(keys)
	for _, k := range keys {
		canon := http.CanonicalHeaderKey(k)
		_, secret := sensitiveHeaders[canon]
		for _, v := range h[k] {
			if secret {
				v = "<redacted>"
			}
			fmt.Fprintf(b, "%s%s: %s\n", prefix, canon, v)
		}
	}
}

// writeBody renders body to b, truncating at [traceBodyLimit]
// with a "... (N more bytes)" suffix when the body is larger.
// Non-UTF-8 bodies are written verbatim — the tracer is for
// debugging, and a corrupt-looking byte sequence is itself a
// signal worth seeing.
//
// encodingLabel, when non-empty, is emitted as a parenthesized
// header line before the body (e.g. "(gzip, 1240 → 8932 bytes)")
// so the operator can see both that compression was on the wire
// and the wire-vs-decoded sizes. Truncation is applied to body
// as passed in, which is the already-decoded payload.
func writeBody(b *strings.Builder, prefix string, body []byte, encodingLabel string) {
	if encodingLabel != "" {
		fmt.Fprintf(b, "%s(%s)\n", prefix, encodingLabel)
	}
	display := body
	truncated := 0
	if len(display) > traceBodyLimit {
		truncated = len(display) - traceBodyLimit
		display = display[:traceBodyLimit]
	}
	for _, line := range strings.SplitAfter(string(display), "\n") {
		if line == "" {
			continue
		}
		b.WriteString(prefix)
		b.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			b.WriteString("\n")
		}
	}
	if truncated > 0 {
		fmt.Fprintf(b, "%s... (%d more bytes)\n", prefix, truncated)
	}
}

// decodeBodyForTrace inspects a Content-Encoding header value
// and returns (a) the bytes to display in the trace and (b) a
// human-readable label summarizing the encoding chain.
//
//   - No encoding (header empty / "identity"): returns body
//     unchanged and an empty label. Caller should not annotate.
//   - Successful decode: returns the decoded payload and a
//     label of the form "gzip, 1240 → 8932 bytes" (or
//     "gzip, br, …" for multi-layer encodings).
//   - Decode failure: returns the original wire bytes and a
//     label naming the encoding that failed plus the error,
//     so the operator can still see *something* even when the
//     stream is corrupt or compressed with an algorithm this
//     build doesn't support.
//
// Per RFC 9110 §8.4, Content-Encoding lists encodings in the
// order they were applied, so this function applies them in
// reverse to undo. Multi-layer encodings are rare on the wire
// but cheap to support.
func decodeBodyForTrace(body []byte, encoding string) (display []byte, label string) {
	chain := make([]string, 0, 2)
	for _, part := range strings.Split(encoding, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "" || p == "identity" {
			continue
		}
		chain = append(chain, p)
	}
	if len(chain) == 0 {
		return body, ""
	}
	chainLabel := strings.Join(chain, ", ")
	cur := body
	for i := len(chain) - 1; i >= 0; i-- {
		next, err := decompressOne(cur, chain[i])
		if err != nil {
			return body, fmt.Sprintf("%s, %d wire bytes (decode failed: %v)", chainLabel, len(body), err)
		}
		cur = next
	}
	return cur, fmt.Sprintf("%s, %d → %d bytes", chainLabel, len(body), len(cur))
}

// decompressOne undoes a single Content-Encoding layer. The
// "identity" entry is filtered out by the caller before we get
// here. Unknown encodings (including zstd, which the wire spec
// allows but neither stdlib nor our brotli dep covers) surface
// as a clear error so the failure label names the offending
// encoding.
func decompressOne(b []byte, enc string) ([]byte, error) {
	switch enc {
	case "gzip", "x-gzip":
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	case "deflate":
		// Some servers (notably IIS) send raw deflate
		// instead of the zlib-wrapped form RFC 9110
		// specifies. Try zlib first via flate.NewReader on
		// the raw stream; on failure fall back to nothing —
		// the caller's failure path surfaces the error.
		r := flate.NewReader(bytes.NewReader(b))
		defer r.Close()
		return io.ReadAll(r)
	case "br":
		return io.ReadAll(brotli.NewReader(bytes.NewReader(b)))
	}
	return nil, fmt.Errorf("unsupported encoding %q", enc)
}

// redactedURL returns raw with values of well-known
// secret-bearing query parameters replaced by "<redacted>".
// Falls back to raw on parse error so a malformed URL is still
// visible in the trace.
func redactedURL(raw string) string {
	i := strings.IndexByte(raw, '?')
	if i < 0 {
		return raw
	}
	head := raw[:i]
	query := raw[i+1:]
	if query == "" {
		return raw
	}
	parts := strings.Split(query, "&")
	for idx, p := range parts {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			continue
		}
		key := p[:eq]
		if _, ok := sensitiveQueryParams[strings.ToLower(key)]; ok {
			parts[idx] = key + "=<redacted>"
		}
	}
	return head + "?" + strings.Join(parts, "&")
}

// formatDuration renders d in a compact form suited to a
// single-line trace: sub-millisecond → µs, sub-second → ms,
// otherwise s. Avoids time.Duration.String's mixed-unit output
// (e.g. "1m2.345s") which is harder to scan in a column.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d.Nanoseconds())/1e3)
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d.Nanoseconds())/1e6)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

