package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- pure retry helpers (no network) ---

func TestShouldRetryStatus(t *testing.T) {
	retry := []int{429, 500, 502, 503, 599}
	noRetry := []int{200, 201, 400, 401, 404, 600}
	for _, c := range retry {
		if !shouldRetryStatus(c) {
			t.Errorf("status %d should be retryable", c)
		}
	}
	for _, c := range noRetry {
		if shouldRetryStatus(c) {
			t.Errorf("status %d should not be retryable", c)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"0", 0},   // non-positive ignored
		{"-3", 0},  // negative ignored
		{"abc", 0}, // HTTP-date form not parsed -> 0
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBackoffDelay_HonorsRetryAfter(t *testing.T) {
	err := retryableErr{apiErr: &APIError{StatusCode: 429}, retryAfter: 7 * time.Second}
	if got := backoffDelay(err, 1, time.Second); got != 7*time.Second {
		t.Errorf("backoffDelay should honor Retry-After: got %v", got)
	}
	// Without Retry-After, attempt 1 base is 1s plus <500ms jitter (production baseUnit).
	d := backoffDelay(errors.New("x"), 1, time.Second)
	if d < time.Second || d >= time.Second+500*time.Millisecond {
		t.Errorf("attempt-1 backoff out of range: %v", d)
	}
}

// TestBackoffDelay_JitterScalesWithBaseUnit pins the W5-review fix: jitter
// must scale with baseUnit (not stay a hardcoded 500ms), otherwise a test
// passing a small baseUnit (e.g. 1ms, via SetRetryTuningForTests) would
// still pay up to 500ms of jitter per retry — defeating the speedup.
func TestBackoffDelay_JitterScalesWithBaseUnit(t *testing.T) {
	d := backoffDelay(errors.New("x"), 1, time.Millisecond)
	if d >= 2*time.Millisecond {
		t.Errorf("attempt-1 backoff with a 1ms baseUnit = %v, want < 2ms (jitter must scale down)", d)
	}
}

// --- httptest-backed client (injectable baseURL) ---

func TestClient_Get_SetsAuthHeaderAndReturnsBody(t *testing.T) {
	var gotKey, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "read-key", "write-key", false, 5*time.Second)
	body, err := c.Get(context.Background(), "/api/user/current", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotKey != "read-key" {
		t.Errorf("read path must use the read key, got %q", gotKey)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept header = %q", gotAccept)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %s", body)
	}
}

func TestClient_PostWrite_UsesWriteKey(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "read-key", "write-key", false, 5*time.Second)
	if _, err := c.PostWrite(context.Background(), "/api/dashboard", map[string]any{"name": "x"}); err != nil {
		t.Fatalf("PostWrite: %v", err)
	}
	if gotKey != "write-key" {
		t.Errorf("write path must use the write key, got %q", gotKey)
	}
}

// TestClient_Get_TimeoutClassifiesAsTimeout pins W7 end-to-end through
// do(): a handler that never responds, combined with a short deadline, must
// surface a *APIError whose Kind() is "timeout" (not the bare, unclassified
// error do() used to return via "max retries exceeded: %w"). Uses a request
// context deadline rather than a short http.Client.Timeout (which the CLI's
// --timeout flag maps to) purely for test speed — a short client.Timeout
// hits the SAME outcome but only after do()'s full 3-retry exponential
// backoff (~7s wall time), since every attempt independently re-times-out;
// a context deadline instead fires do()'s ctx.Done() early-return on the
// very first backoff wait, which is functionally the same classification
// path (ctx.Err() == context.DeadlineExceeded either way).
func TestClient_Get_TimeoutClassifiesAsTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never respond until told to
	}))
	// httptest.Server.Close() blocks until all outstanding requests finish —
	// with a handler that blocks forever, Close() would deadlock unless
	// `block` is closed FIRST. Order matters: this defer must be registered
	// (and therefore run) before any defer that could out-live it.
	defer func() {
		close(block)
		srv.Close()
	}()

	c := New(srv.URL, "k", "", false, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Get(ctx, "/api/dataset", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Kind() != "timeout" {
		t.Errorf("Kind() = %q, want timeout", apiErr.Kind())
	}
	if apiErr.ExitCode() != 1 {
		t.Errorf("ExitCode() = %d, want 1", apiErr.ExitCode())
	}
}

// TestClient_Get_ConnectionRefusedClassifiesAsNetwork pins W7 end-to-end for
// a dead endpoint (server closed, port no longer listening) — the classic
// "forgot dangerouslyDisableSandbox" shape.
//
// Connection-refused isn't status-based, so do()'s retry loop always retries
// it up to maxRetries regardless of tuning — SetRetryTuningForTests(1, 1ms)
// here just makes the wait between attempts (and the number of attempts)
// negligible; it doesn't change what gets asserted (Kind()/ExitCode() are
// computed from the SAME classifyTransportErr(cause) on the SAME underlying
// connection-refused error either way — see the W5 carried-review item this
// pins: production paid ~7.5s of real exponential backoff here on every run).
func TestClient_Get_ConnectionRefusedClassifiesAsNetwork(t *testing.T) {
	defer SetRetryTuningForTests(1, time.Millisecond)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // nothing listens on deadURL anymore -> connection refused

	c := New(deadURL, "k", "", false, 2*time.Second)
	_, err := c.Get(context.Background(), "/api/dataset", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Kind() != "network" {
		t.Errorf("Kind() = %q, want network", apiErr.Kind())
	}
	if apiErr.ExitCode() != 1 {
		t.Errorf("ExitCode() = %d, want 1", apiErr.ExitCode())
	}
}

// TestClient_PostStream_ConnectionRefusedClassifiesAsNetwork pins the
// PostStream path (no retry loop — see the doc comment on PostStream)
// separately from do(): its transport-error branch must also wrap into a
// typed, classified *APIError (carried finding B's prerequisite — the CSV
// export path needs a typed error to route through emitErrorEnvelope).
func TestClient_PostStream_ConnectionRefusedClassifiesAsNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	c := New(deadURL, "k", "", false, 2*time.Second)
	_, err := c.PostStream(context.Background(), "/api/dataset/csv", map[string]any{"q": 1})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Kind() != "network" {
		t.Errorf("Kind() = %q, want network", apiErr.Kind())
	}
}

func TestClient_NonRetryableErrorReturnsAPIError(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "", false, 5*time.Second)
	_, err := c.Get(context.Background(), "/api/card/999", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T %v", err, err)
	}
	if apiErr.Kind() != "not_found" {
		t.Errorf("Kind() = %q, want not_found", apiErr.Kind())
	}
	if hits != 1 {
		t.Errorf("404 must not be retried: %d requests", hits)
	}
}

// TestClient_Get_TruncatedBodyClassifiesAsNetwork pins the review's HIGH
// fix: do() used to discard io.ReadAll's error (respBody, _ := io.ReadAll(...)),
// so a connection that dropped mid-body — cleanly, at a JSON element
// boundary — returned (partialBody, nil) instead of an error. gjson doesn't
// validate strict JSON, so a caller checking e.g. gjson.GetBytes(resp,
// "dashcards").IsArray() would silently get a truncated array back as if it
// were complete (the exact shape that made dashboards add-card's
// GET-then-PUT unsafe: a truncated GET could pass the "dashcards key
// exists" assert with data quietly missing, and the follow-up PUT would
// delete the missing cards). The handler here declares a Content-Length far
// longer than the bytes it actually writes, then closes the connection —
// the client's read gets fewer bytes than promised before EOF, which is
// exactly io.ErrUnexpectedEOF's contract.
func TestClient_Get_TruncatedBodyClassifiesAsNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support hijacking")
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		body := `{"id":42,"dashcards":[{"id":1,"card_id":10}`
		fmt.Fprintf(bufrw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body)+200, body)
		_ = bufrw.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "", false, 2*time.Second)
	_, err := c.Get(context.Background(), "/api/dashboard/42", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError for a truncated body, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 (transport-classified — the truncated response's HTTP 200 must not leak through as a success)", apiErr.StatusCode)
	}
	if apiErr.Kind() != "network" {
		t.Errorf("Kind() = %q, want network (io.ErrUnexpectedEOF)", apiErr.Kind())
	}
}

// TestClient_PostStream_TruncatedErrorBodyClassifiesAsNetwork pins the same
// fix at PostStream's non-2xx drain-for-error-message site (client.go
// ~line 139) — a truncated read while fetching the error body must not
// silently report a status-code error with an empty/partial Body.
func TestClient_PostStream_TruncatedErrorBodyClassifiesAsNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support hijacking")
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		body := `{"error":"bad query"`
		fmt.Fprintf(bufrw, "HTTP/1.1 400 Bad Request\r\nContent-Length: %d\r\n\r\n%s", len(body)+200, body)
		_ = bufrw.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "", false, 2*time.Second)
	_, err := c.PostStream(context.Background(), "/api/dataset/csv", map[string]any{"q": 1})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError for a truncated error body, got %T: %v", err, err)
	}
	if apiErr.Kind() != "network" {
		t.Errorf("Kind() = %q, want network", apiErr.Kind())
	}
}
