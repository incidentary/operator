/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package client contains HTTP clients for the Incidentary v2 API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/incidentary/operator/internal/wireformat"
)

// Defaults for the ingest HTTP client.
const (
	DefaultEndpoint       = "https://api.incidentary.io/api/v2/ingest"
	DefaultAgentVersion   = "0.1.0"
	DefaultTimeout        = 30 * time.Second
	DefaultMaxRetries     = 5
	DefaultInitialBackoff = 500 * time.Millisecond
	DefaultMaxBackoff     = 15 * time.Second
	DefaultMaxRetryWindow = 60 * time.Second

	// HeaderCaptureModeRequested is the response header that instructs the
	// operator (and SDKs) to switch to full-fidelity capture on the next batch.
	HeaderCaptureModeRequested = "X-Capture-Mode-Requested"
	// HeaderAgentVersion identifies the agent version on ingest requests.
	HeaderAgentVersion = "X-Incidentary-Agent-Version"
)

// IngestResponse is the JSON body returned by POST /api/v2/ingest.
type IngestResponse struct {
	Accepted    int            `json:"accepted"`
	Dropped     int            `json:"dropped"`
	DropReasons map[string]int `json:"drop_reasons,omitempty"`
}

// FlushResult is the outcome of a single Flush call: accepted/dropped counts
// plus the value of the X-Capture-Mode-Requested response header, if any.
type FlushResult struct {
	IngestResponse
	CaptureModeRequested string
}

// IngestClient is the contract a callsite needs to send v2 batches. Defined
// here for testability — downstream code depends on this interface, not on
// the concrete struct.
type IngestClient interface {
	Flush(ctx context.Context, batch *wireformat.IngestBatch) (FlushResult, error)
}

// HTTPClient is the concrete HTTP implementation of IngestClient with
// bounded-exponential-backoff retry on 429 and 5xx responses.
type HTTPClient struct {
	endpoint   string
	apiKey     string
	version    string
	httpClient *http.Client

	maxRetries     int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	maxRetryWindow time.Duration
	rand           *rand.Rand
}

// Option configures a HTTPClient via the functional options pattern.
type Option func(*HTTPClient)

// WithEndpoint overrides the ingest endpoint URL.
func WithEndpoint(url string) Option {
	return func(c *HTTPClient) {
		if url != "" {
			c.endpoint = url
		}
	}
}

// WithHTTPClient overrides the underlying *http.Client. Useful for tests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *HTTPClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithAgentVersion overrides the X-Incidentary-Agent-Version header value.
func WithAgentVersion(v string) Option {
	return func(c *HTTPClient) {
		if v != "" {
			c.version = v
		}
	}
}

// WithMaxRetries overrides the retry ceiling.
func WithMaxRetries(n int) Option {
	return func(c *HTTPClient) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// WithRetryBackoff overrides the initial, max, and total-window retry budget.
func WithRetryBackoff(initial, maxBackoff, window time.Duration) Option {
	return func(c *HTTPClient) {
		if initial > 0 {
			c.initialBackoff = initial
		}
		if maxBackoff > 0 {
			c.maxBackoff = maxBackoff
		}
		if window > 0 {
			c.maxRetryWindow = window
		}
	}
}

// NewHTTPClient constructs a HTTPClient. apiKey is the Bearer token used on
// every request. Returns a non-nil client; it is safe to call Flush
// immediately.
func NewHTTPClient(apiKey string, opts ...Option) *HTTPClient {
	c := &HTTPClient{
		endpoint:       DefaultEndpoint,
		apiKey:         apiKey,
		version:        DefaultAgentVersion,
		httpClient:     &http.Client{Timeout: DefaultTimeout},
		maxRetries:     DefaultMaxRetries,
		initialBackoff: DefaultInitialBackoff,
		maxBackoff:     DefaultMaxBackoff,
		maxRetryWindow: DefaultMaxRetryWindow,
		rand:           rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // non-crypto jitter
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Flush marshals the batch and POSTs it to the ingest endpoint, retrying
// transient failures (429, 5xx, network errors) with exponential backoff
// and jitter. Returns the parsed IngestResponse plus the value of the
// X-Capture-Mode-Requested header (if present).
func (c *HTTPClient) Flush(ctx context.Context, batch *wireformat.IngestBatch) (FlushResult, error) {
	if batch == nil {
		return FlushResult{}, errors.New("client: nil batch")
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return FlushResult{}, fmt.Errorf("client: marshal batch: %w", err)
	}

	deadline := time.Now().Add(c.maxRetryWindow)
	backoff := c.initialBackoff

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			// Add jitter: +/- 20% of backoff.
			jitterFactor := 1.0 + (c.rand.Float64()*0.4 - 0.2)
			sleep := min(time.Duration(float64(backoff)*jitterFactor), c.maxBackoff)
			if time.Now().Add(sleep).After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return FlushResult{}, ctx.Err()
			case <-time.After(sleep):
			}
			backoff *= 2
			if backoff > c.maxBackoff {
				backoff = c.maxBackoff
			}
		}

		res, retryable, err := c.doRequest(ctx, body)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !retryable {
			return FlushResult{}, err
		}
		if ctx.Err() != nil {
			return FlushResult{}, ctx.Err()
		}
	}

	if lastErr == nil {
		lastErr = errors.New("client: retry budget exhausted")
	}
	return FlushResult{}, fmt.Errorf("client: flush failed after retries: %w", lastErr)
}

// doRequest performs a single HTTP attempt. Returns (result, retryable, err).
// retryable is true for 429, 5xx, or network errors. For 4xx other than 429
// the error is permanent and retryable=false.
func (c *HTTPClient) doRequest(ctx context.Context, body []byte) (FlushResult, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return FlushResult{}, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set(HeaderAgentVersion, c.version)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network errors are retryable.
		return FlushResult{}, true, fmt.Errorf("post %s: %w", c.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil && readErr != io.EOF {
		return FlushResult{}, true, fmt.Errorf("read response: %w", readErr)
	}

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
		var parsed IngestResponse
		if len(respBody) > 0 {
			if err := json.Unmarshal(respBody, &parsed); err != nil {
				return FlushResult{}, false, fmt.Errorf("parse response: %w", err)
			}
		}
		return FlushResult{
			IngestResponse:       parsed,
			CaptureModeRequested: resp.Header.Get(HeaderCaptureModeRequested),
		}, false, nil

	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return FlushResult{}, true, fmt.Errorf("ingest returned HTTP %d", resp.StatusCode)

	default:
		// 4xx other than 429 — permanent. Include a short body snippet for
		// debugging without leaking secrets (bodies here are server error
		// descriptions, not credentials).
		snippet := string(respBody)
		if len(snippet) > 256 {
			snippet = snippet[:256] + "..."
		}
		return FlushResult{}, false, fmt.Errorf("ingest returned HTTP %d: %s", resp.StatusCode, snippet)
	}
}
