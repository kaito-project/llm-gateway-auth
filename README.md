# LLM Gateway Auth

![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/kaito-project/llm-gateway-auth)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

`llm-gateway-auth` provides authentication and authorization components for an LLM inference gateway built on Istio + Kubernetes Gateway API + [Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/) (InferencePool / InferenceModel). It is part of the [KAITO](https://github.com/kaito-project/kaito) project family.

It plugs into Istio's ingress gateway as an [Envoy `ext_authz`](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_authz_filter) backend and provides two complementary authorization modes for traffic destined to model-serving `InferencePool`s:

- **API-key authz** — validates `sk-…` style keys supplied through `Authorization: Bearer <key>`, `API-Key: <key>`, or `X-API-Key: <key>` (in that precedence order) against `APIKey` Custom Resources managed by an in-cluster operator.
- **Azure / Entra ID authz** — validates Azure AD JWTs (already verified by Istio `RequestAuthentication`) and performs a Kubernetes `SubjectAccessReview` (SAR) to enforce per-namespace inference RBAC.

## Architecture

```
Ingress → Istio Gateway
            ├── RequestAuthentication (JWT)
            ├── EnvoyFilter (reorder JWT before ext_authz)
            └── AuthorizationPolicy ──► ext_authz (apikey | azure)
                                              │
                                              ▼
                                  SAR + API-key validation
                                              │
                                              ▼
                       HTTPRoute → InferencePool → Endpoint Picker → Model
```

The project ships three components:

- **apikey-operator**: Reconciles `APIKey` CRs — generates and rotates keys, manages backing `Secret`s, expires keys, and serves the validating admission webhook.
- **apikey-authz**: Envoy `ext_authz` gRPC server that validates API keys using an in-memory index synced from managed `Secret`s.
- **azure-authz**: Envoy `ext_authz` gRPC server that authorizes Azure AD JWT principals via Kubernetes SAR, with an LRU cache.

See [docs/architecture.md](docs/architecture.md) for the full cluster topology including namespaces, RBAC, and NetworkPolicies.

## Getting Started

### Prerequisites

- Go 1.26+
- `kubectl`, `helm`, `kind` (for local testing)
- Docker or Podman
- An Istio-enabled Kubernetes cluster for end-to-end deployment. The charts do
  not bundle Istio CRDs — `EnvoyFilter`, `PeerAuthentication`,
  `RequestAuthentication`, and `AuthorizationPolicy` must already be
  registered, or `helm install` fails on unknown Kinds.

### Build

```sh
make build              # builds all three binaries into ./bin
make image-build-all    # build all container images
```

Override registry/tag via `OPERATOR_IMG`, `AUTHZ_IMG`, `AZURE_AUTHZ_IMG`. Container runtime is auto-detected; force with `CONTAINER_RUNTIME=podman`.

### Deploy

```sh
make helm-install       # install chart into NAMESPACE (default: llm-gateway)
make helm-upgrade
make helm-uninstall
```

Chart values are documented in [chart/llm-gateway-apikey/values.yaml](chart/llm-gateway-apikey/values.yaml).

### Upgrading expiry enforcement

Roll out the API-key operator before the API-key authz component. The operator
backfills expiry deadlines and key-hash bindings on existing owned Secrets; the
authz component strictly requires this metadata to match the key bytes and fails
closed when it is missing, invalid, or mismatched.

1. Deploy the new `apikey-operator` and wait for its rollout to complete.
2. Confirm active managed Secrets have `kaito.sh/primary-key-expires-at` and
   `kaito.sh/primary-key-hash`. When secondary key data exists, also confirm
   `kaito.sh/secondary-key-expires-at` and `kaito.sh/secondary-key-hash`.
3. Deploy the new `apikey-authz` component.

If both components are upgraded simultaneously, new authz replicas can observe
a legacy Secret before the operator backfills it. Requests using that Secret
will be denied transiently until reconciliation completes; this is intentional
fail-closed behavior.

Rolling back only the operator can leave the fixed authz component observing key
bytes written by old code without matching hash bindings. Those credentials are
denied until a fixed operator reconciles or replaces the Secret. Old operators
may remove secondary key bytes without removing the newer secondary annotations;
the authz component ignores that stale secondary metadata and continues to use a
valid, correctly bound primary key.

## Contributing

This project welcomes contributions and suggestions. Contributions require you to agree to a Contributor License Agreement (CLA). For details, visit [CLAs for CNCF](https://github.com/cncf/cla?tab=readme-ov-file).

## Get Involved!

- Visit [#KAITO channel in CNCF Slack](https://cloud-native.slack.com/archives/C09B4EWCZ5M) to discuss features and proposals.

## License

See [Apache License 2.0](LICENSE).

## Code of Conduct

This project has adopted the [Cloud Native Compute Foundation Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md).

## Contact

- Please send emails to "KAITO devs" <kaito-dev@microsoft.com> for any issues.
