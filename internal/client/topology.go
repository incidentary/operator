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

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DefaultTopologyEndpoint is the Incidentary API path for topology reports.
const DefaultTopologyEndpoint = "https://api.incidentary.com/api/v2/workspace/topology"

// TopologyWorkload is a single workload entry in a topology report.
type TopologyWorkload struct {
	ServiceID         string            `json:"service_id"`
	ServiceIDSource   string            `json:"service_id_source"`
	Kind              string            `json:"kind"`
	Namespace         string            `json:"namespace"`
	Replicas          int32             `json:"replicas"`
	AvailableReplicas int32             `json:"available_replicas"`
	Image             string            `json:"image,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	CreatedAt         int64             `json:"created_at"`
	Conditions        []WorkloadCond    `json:"conditions,omitempty"`
}

// WorkloadCond is a subset of metav1.Condition — Type/Status only.
type WorkloadCond struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// TopologyReport is the POST /api/v2/workspace/topology body.
type TopologyReport struct {
	ClusterName string             `json:"cluster_name"`
	ObservedAt  int64              `json:"observed_at"` // Unix nanoseconds
	Workloads   []TopologyWorkload `json:"workloads"`
}

// TopologyResponse is the server's reply.
type TopologyResponse struct {
	Accepted             int `json:"accepted"`
	CreatedGhostServices int `json:"created_ghost_services"`
	UpdatedServices      int `json:"updated_services"`
}

// TopologyClient sends topology reports to the Incidentary API.
type TopologyClient interface {
	Report(ctx context.Context, report *TopologyReport) (*TopologyResponse, error)
}

// HTTPTopologyClient is the HTTP implementation of TopologyClient. It shares
// the retry/backoff tuning of HTTPClient via embedded reuse.
type HTTPTopologyClient struct {
	endpoint   string
	apiKey     string
	version    string
	httpClient *http.Client
}

// NewTopologyClient constructs a TopologyClient using the default endpoint
// or the one overridden via WithTopologyEndpoint.
func NewTopologyClient(apiKey string, opts ...TopologyOption) *HTTPTopologyClient {
	c := &HTTPTopologyClient{
		endpoint:   DefaultTopologyEndpoint,
		apiKey:     apiKey,
		version:    DefaultAgentVersion,
		httpClient: &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// TopologyOption configures an HTTPTopologyClient.
type TopologyOption func(*HTTPTopologyClient)

// WithTopologyEndpoint overrides the topology endpoint URL.
func WithTopologyEndpoint(url string) TopologyOption {
	return func(c *HTTPTopologyClient) {
		if url != "" {
			c.endpoint = url
		}
	}
}

// WithTopologyHTTPClient overrides the underlying *http.Client.
func WithTopologyHTTPClient(hc *http.Client) TopologyOption {
	return func(c *HTTPTopologyClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// Report sends the topology report. Returns the parsed response on success.
// 2xx responses are parsed; non-2xx are returned as errors with the body.
func (c *HTTPTopologyClient) Report(ctx context.Context, report *TopologyReport) (*TopologyResponse, error) {
	if report == nil {
		return nil, errors.New("client: nil topology report")
	}
	body, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("client: marshal topology report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("client: build topology request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set(HeaderAgentVersion, c.version)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: post topology: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(respBody)
		if len(snippet) > 256 {
			snippet = snippet[:256] + "..."
		}
		return nil, fmt.Errorf("topology HTTP %d: %s", resp.StatusCode, snippet)
	}
	var parsed TopologyResponse
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return nil, fmt.Errorf("client: parse topology response: %w", err)
		}
	}
	return &parsed, nil
}
