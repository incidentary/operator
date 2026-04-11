# incidentary-operator

Kubernetes operator for Incidentary — watches cluster events and reports infrastructure telemetry to the Incidentary v2 ingest API.

Status: Alpha — under active development.

See https://incidentary.com/ for the Incidentary platform.

## Development

Prerequisites:

- Go 1.25+
- Docker with Buildx
- kubectl

Common commands:

```
make build        # compile the manager binary
make test         # run unit tests (downloads envtest binaries on first run)
make lint         # run golangci-lint
make manifests    # regenerate CRD YAML from kubebuilder markers
make generate     # regenerate DeepCopy methods
make run          # run the manager against the current kubeconfig
```

## Quick start

Helm install (placeholder — chart scaffolded in Phase 5):

```
helm install incidentary-operator ./charts/incidentary-operator \
  --namespace incidentary-system --create-namespace
```

## License

Apache 2.0.
