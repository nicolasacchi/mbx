// Package client wraps the Metabase API.
//
// Auth: X-API-Key header.
// Optional separate write key (MBX_WRITE_API_KEY) for mutating commands.
// HTTP/2 multiplexing enabled (saves ~150-200ms on parallel `overview`).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

const (
	DefaultTimeout = 60 * time.Second // Metabase queries can be slow
	MaxRetries     = 3
)

type Client struct {
	http           *http.Client
	apiKey         string
	writeAPIKey    string
	baseURL        string
	verbose        bool
	timing         bool
	maxRetries     int
	retryBaseDelay time.Duration
}

// New constructs a Client. writeAPIKey may be empty (falls back to apiKey).
// timeout=0 falls back to DefaultTimeout.
//
// Transport is tuned for HTTP/2 multiplexing — Metabase serves h2 over ALPN,
// so 7 parallel goroutines in `overview` collapse onto 1 TCP/TLS connection
// instead of opening 5-6 separate ones. Set MBX_HTTP1_ONLY=1 to fall back
// to HTTP/1.1 if HTTP/2 misbehaves.
//
// Retry count/backoff base default to the production values (MaxRetries, 1s)
// unless a test has called SetRetryTuningForTests — see that function's doc
// comment for why this exists.
func New(baseURL, apiKey, writeAPIKey string, verbose bool, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	transport := &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   os.Getenv("MBX_HTTP1_ONLY") == "",
	}
	maxRetries, retryBaseDelay := MaxRetries, time.Second
	if testRetryTuning != nil {
		maxRetries, retryBaseDelay = testRetryTuning.maxRetries, testRetryTuning.baseDelay
	}
	return &Client{
		http:           &http.Client{Timeout: timeout, Transport: transport},
		apiKey:         apiKey,
		writeAPIKey:    writeAPIKey,
		baseURL:        baseURL,
		verbose:        verbose,
		maxRetries:     maxRetries,
		retryBaseDelay: retryBaseDelay,
	}
}

// testRetryTuning overrides every subsequent New() call's retry count/backoff
// base — nil in production (no production code path sets it). Exists so
// tests exercising the transport-failure/retry path (connection-refused,
// network-class doctor probes) don't pay do()'s full worst-case backoff
// (~7.5s: 1s+2s+4s plus jitter across 3 retries) on every run, including
// commands-package tests that reach client construction indirectly through
// getClient() rather than calling New directly. Package-global by design —
// safe because this repo's tests never run with t.Parallel() (verified via
// repo-wide grep); a future parallel test touching this would need its own
// synchronization.
var testRetryTuning *retryTuning

type retryTuning struct {
	maxRetries int
	baseDelay  time.Duration
}

// SetRetryTuningForTests overrides the retry count and backoff base every
// subsequent New() call uses. Returns a restore func — callers should
// `defer restore()` (or wire it through t.Cleanup) to scope the override to
// one test. Not for production use.
func SetRetryTuningForTests(maxRetries int, baseDelay time.Duration) (restore func()) {
	prev := testRetryTuning
	testRetryTuning = &retryTuning{maxRetries: maxRetries, baseDelay: baseDelay}
	return func() { testRetryTuning = prev }
}

func (c *Client) BaseURL() string { return c.baseURL }

// HasWriteKey reports whether a separate write key is configured.
func (c *Client) HasWriteKey() bool { return c.writeAPIKey != "" }

// SetTiming enables per-call wall-time logging to stderr.
func (c *Client) SetTiming(on bool) { c.timing = on }

// Prewarm fires a best-effort GET /api/health to warm the TLS connection
// pool. Used by PersistentPreRun. Errors are swallowed.
func (c *Client) Prewarm(ctx context.Context) {
	_, _ = c.Get(ctx, "/api/health", nil)
}

// Get performs a GET request.
func (c *Client) Get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, params, nil, false)
}

// Post performs a POST with a JSON body.
func (c *Client) Post(ctx context.Context, path string, body any) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, nil, body, false)
}

// PostQuery is a read-only POST (e.g. /api/dataset for native queries).
func (c *Client) PostQuery(ctx context.Context, path string, body any) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, nil, body, false)
}

// PostWrite uses the write API key (or falls back to read key).
func (c *Client) PostWrite(ctx context.Context, path string, body any) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, nil, body, true)
}

// PutWrite uses the write API key.
func (c *Client) PutWrite(ctx context.Context, path string, body any) ([]byte, error) {
	return c.do(ctx, http.MethodPut, path, nil, body, true)
}

// PostStream issues a POST and returns the response body as an io.ReadCloser
// for the caller to drain. Caller MUST Close. No retry — streaming bodies
// can't be replayed. Used for large CSV/XLSX exports to avoid buffering
// hundreds of MB in memory.
func (c *Client) PostStream(ctx context.Context, path string, body any) (io.ReadCloser, error) {
	full := c.baseURL + path
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if len(bodyBytes) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.verbose || c.timing {
		fmt.Fprintf(verboseDest, "mbx: POST %s (stream)\n", full)
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		// No retry loop here (see the doc comment above), so this is the
		// only place a PostStream transport failure can be classified —
		// wrap it the same way do() does so Kind() sees "timeout"/"network"
		// instead of falling through to "client_error" (W7).
		return nil, &APIError{StatusCode: 0, Method: http.MethodPost, URL: full, Cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain to populate body for the error envelope, then close.
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			// The read itself failed (connection dropped mid-body, etc.) —
			// don't silently report an error with an empty/truncated Body,
			// which would hide that this was a transport failure, not a
			// real (if unreadable) API error. Classify via Kind() the same
			// way a dial failure would (StatusCode 0).
			return nil, &APIError{StatusCode: 0, Method: http.MethodPost, URL: full, Cause: readErr}
		}
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Method:     http.MethodPost,
			URL:        full,
			Body:       string(respBody),
		}
	}
	if c.timing {
		fmt.Fprintf(timingDest, "mbx: POST %s — wire-headers=%dms (streaming body)\n", full, time.Since(start).Milliseconds())
	}
	return resp.Body, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, useWriteKey bool) ([]byte, error) {
	full := c.baseURL + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}

	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
	}

	key := c.apiKey
	if useWriteKey && c.writeAPIKey != "" {
		key = c.writeAPIKey
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoffDelay(lastErr, attempt, c.retryBaseDelay)
			if c.verbose {
				fmt.Fprintf(verboseDest, "mbx: retry %d/%d after %s\n", attempt, c.maxRetries, delay)
			}
			select {
			case <-ctx.Done():
				// Immediate, non-retryable — no more attempts follow, so
				// classify right here (W7) rather than returning the bare
				// context error with no Kind()/ExitCode().
				return nil, &APIError{StatusCode: 0, Method: method, URL: full, Cause: ctx.Err()}
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, full, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-API-Key", key)
		req.Header.Set("Accept", "application/json")
		if len(bodyBytes) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}

		if c.verbose {
			fmt.Fprintf(verboseDest, "mbx: %s %s\n", method, full)
		}

		start := time.Now()
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			// A short/truncated read here is dangerous for any caller that
			// needs the FULL body — gjson is lenient on malformed/
			// incomplete JSON and would silently return e.g. a dashcards
			// array missing its tail elements rather than erroring
			// (dashboards add-card's mandatory GET is exactly this case:
			// a truncated 200 could otherwise pass its "dashcards key
			// exists" assert with data quietly missing, and the follow-up
			// PUT would delete the missing cards). Fail loudly instead of
			// falling through with a partial respBody — same StatusCode-0
			// classification a dial failure gets, no retry (the request
			// itself succeeded; retrying wouldn't be safe for a write's
			// read step without re-deriving idempotency).
			return nil, &APIError{StatusCode: 0, Method: method, URL: full, Cause: readErr}
		}
		wallMS := time.Since(start).Milliseconds()

		if c.verbose {
			fmt.Fprintf(verboseDest, "mbx: -> %d (%d bytes)\n", resp.StatusCode, len(respBody))
		}
		if c.timing {
			serverMS := extractServerRunningMS(respBody)
			if serverMS >= 0 {
				fmt.Fprintf(timingDest, "mbx: %s %s — server=%dms wall=%dms\n", method, path, serverMS, wallMS)
			} else {
				fmt.Fprintf(timingDest, "mbx: %s %s — wall=%dms\n", method, path, wallMS)
			}
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}

		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			URL:        full,
			Body:       string(respBody),
		}
		if shouldRetryStatus(resp.StatusCode) && attempt < c.maxRetries {
			lastErr = retryableErr{apiErr: apiErr, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
			continue
		}
		return nil, apiErr
	}

	// Reaching here means every attempt failed at the transport level (a
	// status-code-based retry either succeeds, gets retried again below
	// MaxRetries, or returns its *APIError directly above — it never falls
	// out of the loop) — so lastErr is always the raw error from the last
	// failed c.http.Do(req), never a retryableErr. Wrap it the same way the
	// ctx.Done() branch above does, so Kind() classifies it (W7) instead of
	// leaving a bare, unclassified error.
	return nil, fmt.Errorf("max retries exceeded: %w", &APIError{StatusCode: 0, Method: method, URL: full, Cause: lastErr})
}

// extractServerRunningMS pulls Metabase's `running_time` from a JSON response
// without doing a full unmarshal. Returns -1 if absent.
func extractServerRunningMS(body []byte) int64 {
	// Cheap substring probe to avoid full JSON parse on every call.
	idx := bytes.Index(body, []byte(`"running_time":`))
	if idx < 0 {
		return -1
	}
	rest := body[idx+len(`"running_time":`):]
	end := bytes.IndexAny(rest, ",}")
	if end < 0 {
		return -1
	}
	num, err := strconv.ParseInt(string(bytes.TrimSpace(rest[:end])), 10, 64)
	if err != nil {
		return -1
	}
	return num
}

type retryableErr struct {
	apiErr     *APIError
	retryAfter time.Duration
}

func (r retryableErr) Error() string { return r.apiErr.Error() }

// backoffDelay computes the exponential-backoff wait before retry `attempt`
// (1-indexed), doubling baseUnit each attempt plus up to baseUnit/2 of
// jitter. Production always calls this with baseUnit=1s (giving the
// documented 1s/2s/4s-plus-jitter progression); tests can pass a much
// smaller baseUnit via Client.retryBaseDelay (see SetRetryTuningForTests)
// so the jitter — proportional to baseUnit, not a hardcoded 500ms — stays
// negligible instead of dominating a deliberately-shortened test delay.
func backoffDelay(lastErr error, attempt int, baseUnit time.Duration) time.Duration {
	if r, ok := lastErr.(retryableErr); ok && r.retryAfter > 0 {
		return r.retryAfter
	}
	base := time.Duration(1<<(attempt-1)) * baseUnit
	jitter := time.Duration(rand.Int63n(int64(baseUnit/2) + 1))
	return base + jitter
}

func shouldRetryStatus(code int) bool {
	return code == 429 || (code >= 500 && code < 600)
}

func parseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

var (
	verboseDest io.Writer = io.Discard
	timingDest  io.Writer = io.Discard
)

// SetVerboseDest routes verbose output (default: io.Discard).
func SetVerboseDest(w io.Writer) { verboseDest = w }

// SetTimingDest routes timing output (default: io.Discard).
func SetTimingDest(w io.Writer) { timingDest = w }
