/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	ingestclient "github.com/incidentary/operator/internal/client"
)

// -----------------------------------------------------------------------------
// Environment variable parsing.
//
// All env-var-handling helpers live here so the wiring in main() stays focused
// on assembling the controller-runtime Manager and its runnables.
// -----------------------------------------------------------------------------

// getEnvOrDefault returns the value of an environment variable or fallback
// when unset or empty.
func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseStringSliceEnv reads a comma-separated env var into a string slice.
// Returns nil (not an empty slice) when the var is unset, blank, or contains
// only whitespace so callers can distinguish "not configured" from
// "explicitly configured empty". Whitespace around each element is trimmed
// and blank entries are dropped, so " ns1 , , ns2 " yields ["ns1", "ns2"].
func parseStringSliceEnv(key string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// parseDurationEnv reads an integer-seconds duration from an environment
// variable, falling back to the supplied default when unset or invalid.
func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return fallback
	}
	return time.Duration(secs) * time.Second
}

// -----------------------------------------------------------------------------
// Configuration warnings.
//
// These predicates report misconfigurations that don't prevent startup but
// will silently break telemetry. main() consumes them to log structured
// startup warnings.
// -----------------------------------------------------------------------------

// warnIfMisconfigured returns true when an API key is set without a matching
// workspace ID. In that state the ingest server will reject every batch with
// 422 WORKSPACE_MISMATCH, silently discarding all events. The caller is
// responsible for logging; this function has no side effects.
func warnIfMisconfigured(apiKey, workspaceID string) bool {
	return apiKey != "" && workspaceID == ""
}

// warnIfClusterNameUnset returns true when the cluster name has not been
// explicitly configured. §5.1 of wire format v2 requires k8s.cluster.name
// for k8s_operator agents; sending the placeholder "unknown" is technically
// compliant but makes cluster-level correlation meaningless in the backend.
func warnIfClusterNameUnset(clusterName string) bool {
	return clusterName == "" || clusterName == "unknown"
}

// -----------------------------------------------------------------------------
// Bootstrap Provider construction.
// -----------------------------------------------------------------------------

// buildInitialProvider constructs a client.Provider seeded with bootstrap
// credentials from environment variables. The IncidentaryConfigReconciler
// will Rotate() this Provider on every successful reconciliation, replacing
// these initial clients with whatever the CR-referenced Secret contains.
//
// When apiKey is empty, the Provider is seeded with dropping stubs so the
// operator runs in a no-op state until the controller reconciles a CR.
func buildInitialProvider(
	apiKey, workspaceID, ingestEP, topologyEP, servicesEP string,
	log logr.Logger,
) *ingestclient.Provider {
	// Construct with valid clients first (NewProvider rejects nil).
	p := ingestclient.NewProvider(
		ingestclient.NewHTTPClient("bootstrap", ingestclient.WithEndpoint(ingestEP)),
		ingestclient.NewTopologyClient("bootstrap", ingestclient.WithTopologyEndpoint(topologyEP)),
		ingestclient.NewServicesClient("bootstrap", ingestclient.WithServicesEndpoint(servicesEP)),
	)
	// Then immediately Rotate to the actual configuration. When apiKey is
	// empty this installs dropping stubs; otherwise it installs real HTTP
	// clients with the configured key. Either way the bootstrap clients
	// above are discarded before any flush can happen.
	p.Rotate(apiKey, workspaceID, ingestEP, topologyEP, servicesEP, log)
	return p
}
