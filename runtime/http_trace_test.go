package runtime_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/partite-ai/particles/runtime"
)

// gzipBytes returns s gzipped — used to construct compressed
// response/request bodies in the decode tests below.
func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func deflateBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate.NewWriter: %v", err)
	}
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

func brotliBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

// stubDoer is a minimal HTTPDoer that returns a canned response
// (or error) and records the request URL + body bytes it saw.
type stubDoer struct {
	resp    *http.Response
	err     error
	gotURL  string
	gotBody []byte
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	s.gotURL = req.URL.String()
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		s.gotBody = b
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

// newResp builds an *http.Response with the given status code,
// headers, and body — populated from in-memory bytes so tests
// don't need an httptest server.
func newResp(status int, body string, hdr http.Header) *http.Response {
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func mustReq(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

// fixedClock returns a function suitable for TracingHTTPDoer.Now
// that walks forward by step on each call. The first call
// returns start; the second start+step; etc. We use this to make
// the duration column deterministic across runs.
func fixedClock(start time.Time, step time.Duration) func() time.Time {
	var calls int
	return func() time.Time {
		t := start.Add(time.Duration(calls) * step)
		calls++
		return t
	}
}

func TestTrace_Basic_OneLinePerDirection(t *testing.T) {
	var buf bytes.Buffer
	stub := &stubDoer{resp: newResp(200, "ok", nil)}
	tr := &runtime.TracingHTTPDoer{
		Inner: stub,
		W:     &buf,
		Level: runtime.TraceBasic,
		Now:   fixedClock(time.Unix(0, 0), 5*time.Millisecond),
	}

	if _, err := tr.Do(mustReq(t, "GET", "https://api.example.com/users/42", "")); err != nil {
		t.Fatalf("Do: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "--> GET https://api.example.com/users/42") {
		t.Errorf("missing request line; got:\n%s", out)
	}
	if !strings.Contains(out, "<-- GET https://api.example.com/users/42  200") {
		t.Errorf("missing response line; got:\n%s", out)
	}
	if !strings.Contains(out, "5.0ms") {
		t.Errorf("missing duration; got:\n%s", out)
	}
	// Basic level must not emit headers or bodies.
	if strings.Contains(out, "Content-Type") {
		t.Errorf("basic level leaked headers; got:\n%s", out)
	}
}

func TestTrace_Headers_RedactsAuthorization(t *testing.T) {
	var buf bytes.Buffer
	stub := &stubDoer{
		resp: newResp(204, "", http.Header{
			"Content-Type": []string{"application/json"},
			"Set-Cookie":   []string{"sid=super-secret-cookie"},
		}),
	}
	tr := &runtime.TracingHTTPDoer{
		Inner: stub,
		W:     &buf,
		Level: runtime.TraceHeaders,
		Now:   fixedClock(time.Unix(0, 0), time.Millisecond),
	}

	req := mustReq(t, "POST", "https://api.example.com/x", "")
	req.Header.Set("Authorization", "Bearer ya29.real-token-bytes")
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("X-Trace-Id", "req-42")

	if _, err := tr.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"Authorization: <redacted>",
		"Cookie: <redacted>",
		"Set-Cookie: <redacted>",
		"X-Trace-Id: req-42",
		"Content-Type: application/json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
	for _, leak := range []string{
		"ya29.real-token-bytes",
		"session=abc",
		"super-secret-cookie",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaked secret %q\ngot:\n%s", leak, out)
		}
	}
}

func TestTrace_RedactsSensitiveQueryParams(t *testing.T) {
	var buf bytes.Buffer
	stub := &stubDoer{resp: newResp(200, "", nil)}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceBasic}

	req := mustReq(t, "GET", "https://api.example.com/q?api_key=SECRETKEY&name=foo&token=ALSO", "")
	if _, err := tr.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "SECRETKEY") || strings.Contains(out, "ALSO") {
		t.Errorf("secret query param leaked; got:\n%s", out)
	}
	if !strings.Contains(out, "api_key=<redacted>") {
		t.Errorf("api_key redaction missing; got:\n%s", out)
	}
	if !strings.Contains(out, "token=<redacted>") {
		t.Errorf("token redaction missing; got:\n%s", out)
	}
	if !strings.Contains(out, "name=foo") {
		t.Errorf("non-sensitive param dropped; got:\n%s", out)
	}
	// The downstream doer should still have seen the real
	// values — the tracer's redaction is a logging concern,
	// not a request-mutation one.
	if !strings.Contains(stub.gotURL, "SECRETKEY") {
		t.Errorf("tracer mutated the real request URL: %q", stub.gotURL)
	}
}

func TestTrace_Full_IncludesBodiesAndTruncates(t *testing.T) {
	respBody := strings.Repeat("X", 5000) // > traceBodyLimit (4096)
	var buf bytes.Buffer
	stub := &stubDoer{resp: newResp(200, respBody, http.Header{"Content-Type": []string{"text/plain"}})}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceFull}

	reqBody := `{"hello":"world"}`
	req := mustReq(t, "POST", "https://api.example.com/echo", reqBody)
	req.Header.Set("Content-Type", "application/json")

	resp, err := tr.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	// Downstream doer must still receive the original request
	// body — the tracer buffers it but doesn't drop it.
	if string(stub.gotBody) != reqBody {
		t.Errorf("downstream lost request body; got %q want %q", stub.gotBody, reqBody)
	}

	// Caller-side response body must also be intact — the
	// tracer buffers, then re-wraps.
	gotResp, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read resp body: %v", err)
	}
	if string(gotResp) != respBody {
		t.Errorf("caller saw truncated response: %d bytes want %d", len(gotResp), len(respBody))
	}

	out := buf.String()
	if !strings.Contains(out, reqBody) {
		t.Errorf("request body missing from trace; got:\n%s", out)
	}
	if !strings.Contains(out, "... (904 more bytes)") {
		t.Errorf("expected truncation marker for 5000-byte body (4096 cap), got:\n%s", out)
	}
}

func TestTrace_LogsErrorFromInnerDoer(t *testing.T) {
	var buf bytes.Buffer
	stubErr := errors.New("dial: connection refused")
	stub := &stubDoer{err: stubErr}
	tr := &runtime.TracingHTTPDoer{
		Inner: stub,
		W:     &buf,
		Level: runtime.TraceBasic,
		Now:   fixedClock(time.Unix(0, 0), 2*time.Millisecond),
	}

	_, err := tr.Do(mustReq(t, "GET", "https://api.example.com/down", ""))
	if !errors.Is(err, stubErr) {
		t.Fatalf("Do returned %v, want %v", err, stubErr)
	}
	out := buf.String()
	if !strings.Contains(out, "ERROR") || !strings.Contains(out, "connection refused") {
		t.Errorf("error line missing/incomplete; got:\n%s", out)
	}
}

func TestTrace_ConcurrentRequestsDoNotInterleaveLines(t *testing.T) {
	// With 50 concurrent requests at TraceHeaders, each
	// emitting multi-line output, the mutex in writeLine must
	// keep each request's request-line + response-line block
	// contiguous. Easiest check: every line in the buffer is
	// well-formed (no two emit() prefixes on one line).
	var buf bytes.Buffer
	stub := &stubDoer{resp: newResp(200, "", http.Header{"X-Marker": []string{"yes"}})}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceHeaders}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = tr.Do(mustReq(t, "GET", "https://api.example.com/x", ""))
		}()
	}
	wg.Wait()

	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		// Each line begins with one of the recognized prefixes
		// or is an indented header line.
		if !(strings.HasPrefix(line, "--> ") ||
			strings.HasPrefix(line, "<-- ") ||
			strings.HasPrefix(line, "    ")) {
			t.Errorf("malformed line (likely interleaved): %q", line)
		}
	}
}

// newRespRaw is newResp's binary-body sibling, used by the
// compression tests below where the on-wire payload isn't UTF-8.
func newRespRaw(status int, body []byte, hdr http.Header) *http.Response {
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     hdr,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func TestTrace_Full_DecodesGzippedResponse(t *testing.T) {
	plaintext := `{"users":[{"id":1,"name":"alice"},{"id":2,"name":"bob"}]}`
	wire := gzipBytes(t, plaintext)
	var buf bytes.Buffer
	stub := &stubDoer{resp: newRespRaw(200, wire, http.Header{
		"Content-Type":     []string{"application/json"},
		"Content-Encoding": []string{"gzip"},
	})}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceFull}

	resp, err := tr.Do(mustReq(t, "GET", "https://api.example.com/users", ""))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	// Caller still receives the original *compressed* wire bytes —
	// the tracer must not alter Content-Encoding semantics.
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, wire) {
		t.Errorf("caller saw mutated body (tracer decompressed in place):\nlen=%d want %d", len(got), len(wire))
	}

	out := buf.String()
	if !strings.Contains(out, plaintext) {
		t.Errorf("decoded plaintext missing from trace; got:\n%s", out)
	}
	wantLabel := "(gzip, " // exact byte sizes vary by gzip impl — just match the prefix
	if !strings.Contains(out, wantLabel) {
		t.Errorf("missing gzip size-marker label; got:\n%s", out)
	}
	if !strings.Contains(out, "→") || !strings.Contains(out, "bytes)") {
		t.Errorf("size-marker label malformed; got:\n%s", out)
	}
}

func TestTrace_Full_DecodesBrotliResponse(t *testing.T) {
	// Real-world motivator: Google APIs (sheets, drive) frequently
	// respond with Content-Encoding: br.
	plaintext := strings.Repeat(`{"row":["a","b","c"]},`, 20)
	wire := brotliBytes(t, plaintext)
	var buf bytes.Buffer
	stub := &stubDoer{resp: newRespRaw(200, wire, http.Header{
		"Content-Encoding": []string{"br"},
	})}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceFull}

	if _, err := tr.Do(mustReq(t, "GET", "https://sheets.googleapis.com/v4/x", "")); err != nil {
		t.Fatalf("Do: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"row":["a","b","c"]`) {
		t.Errorf("brotli body not decoded; got:\n%s", out)
	}
	if !strings.Contains(out, "(br, ") {
		t.Errorf("missing brotli label; got:\n%s", out)
	}
}

func TestTrace_Full_DecodesDeflateResponse(t *testing.T) {
	plaintext := "deflate streams are rare on the wire but spec-allowed"
	wire := deflateBytes(t, plaintext)
	var buf bytes.Buffer
	stub := &stubDoer{resp: newRespRaw(200, wire, http.Header{"Content-Encoding": []string{"deflate"}})}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceFull}

	if _, err := tr.Do(mustReq(t, "GET", "https://api.example.com/x", "")); err != nil {
		t.Fatalf("Do: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, plaintext) {
		t.Errorf("deflate body not decoded; got:\n%s", out)
	}
	if !strings.Contains(out, "(deflate, ") {
		t.Errorf("missing deflate label; got:\n%s", out)
	}
}

func TestTrace_Full_DecodesChainedEncodings(t *testing.T) {
	// `Content-Encoding: gzip, br` per RFC 9110 §8.4 means
	// gzip was applied first, then br on top — to decode we
	// undo br first, then gzip.
	plaintext := `{"chained":"encoding"}`
	innerWire := gzipBytes(t, plaintext)
	outerWire := brotliBytes(t, string(innerWire))
	var buf bytes.Buffer
	stub := &stubDoer{resp: newRespRaw(200, []byte(outerWire), http.Header{
		"Content-Encoding": []string{"gzip, br"},
	})}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceFull}

	if _, err := tr.Do(mustReq(t, "GET", "https://api.example.com/x", "")); err != nil {
		t.Fatalf("Do: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, plaintext) {
		t.Errorf("chained encoding not fully decoded; got:\n%s", out)
	}
	if !strings.Contains(out, "(gzip, br, ") {
		t.Errorf("chained label missing; got:\n%s", out)
	}
}

func TestTrace_Full_FailedDecodeFallsBack(t *testing.T) {
	// Server claims gzip but the bytes aren't a valid gzip
	// stream. The tracer must (a) still emit a trace line, (b)
	// surface the failure so the operator knows why the body
	// looks like binary garbage, and (c) leave the caller's
	// response body intact.
	wire := []byte("this is not actually gzipped at all")
	var buf bytes.Buffer
	stub := &stubDoer{resp: newRespRaw(200, wire, http.Header{"Content-Encoding": []string{"gzip"}})}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceFull}

	resp, err := tr.Do(mustReq(t, "GET", "https://api.example.com/x", ""))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, wire) {
		t.Errorf("caller body mutated on decode failure")
	}
	out := buf.String()
	if !strings.Contains(out, "decode failed") {
		t.Errorf("failure label missing; got:\n%s", out)
	}
	if !strings.Contains(out, "gzip") {
		t.Errorf("failure label should name the offending encoding; got:\n%s", out)
	}
	if !strings.Contains(out, "this is not actually gzipped") {
		t.Errorf("raw bytes should still be shown on decode failure; got:\n%s", out)
	}
}

func TestTrace_Full_IdentityEncodingNotLabeled(t *testing.T) {
	// Content-Encoding: identity is the no-op encoding. It
	// shouldn't trigger a decode pass and shouldn't add a
	// label — the body is already in the clear.
	var buf bytes.Buffer
	stub := &stubDoer{resp: newRespRaw(200, []byte(`{"ok":true}`), http.Header{
		"Content-Encoding": []string{"identity"},
	})}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceFull}

	if _, err := tr.Do(mustReq(t, "GET", "https://api.example.com/x", "")); err != nil {
		t.Fatalf("Do: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `{"ok":true}`) {
		t.Errorf("identity body missing from trace; got:\n%s", out)
	}
	if strings.Contains(out, "(identity") || strings.Contains(out, "→") {
		t.Errorf("identity encoding should not emit a size label; got:\n%s", out)
	}
}

func TestTrace_Full_DecodesGzippedRequestBody(t *testing.T) {
	plaintext := `{"batch":[1,2,3,4,5]}`
	wire := gzipBytes(t, plaintext)
	var buf bytes.Buffer
	stub := &stubDoer{resp: newResp(200, "", nil)}
	tr := &runtime.TracingHTTPDoer{Inner: stub, W: &buf, Level: runtime.TraceFull}

	req := mustReq(t, "POST", "https://api.example.com/batch", string(wire))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")

	if _, err := tr.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	// Downstream still gets the on-wire gzipped bytes.
	if !bytes.Equal(stub.gotBody, wire) {
		t.Errorf("downstream got decoded request body; want on-wire bytes")
	}
	out := buf.String()
	if !strings.Contains(out, plaintext) {
		t.Errorf("request body not decoded in trace; got:\n%s", out)
	}
	if !strings.Contains(out, "(gzip, ") {
		t.Errorf("request gzip label missing; got:\n%s", out)
	}
}

func TestParseTraceLevel(t *testing.T) {
	cases := []struct {
		in   string
		want runtime.TraceLevel
		err  bool
	}{
		{"", runtime.TraceBasic, false},
		{"basic", runtime.TraceBasic, false},
		{"BASIC", runtime.TraceBasic, false},
		{"  headers ", runtime.TraceHeaders, false},
		{"full", runtime.TraceFull, false},
		{"verbose", runtime.TraceOff, true},
	}
	for _, c := range cases {
		got, err := runtime.ParseTraceLevel(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseTraceLevel(%q): want error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTraceLevel(%q): unexpected error %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseTraceLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
