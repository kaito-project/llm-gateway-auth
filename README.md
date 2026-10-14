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
- **apikey-authz**: Envoy `ext_authz` gRPC server that validates API keys using an in-memory index joined from active `APIKey` status and each resource's declared managed `Secret`.
- **azure-authz**: Envoy `ext_authz` gRPC server that authorizes Azure AD JWT principals via Kubernetes SAR, with an LRU cache.

See [docs/architecture.md](docs/architecture.md) for the full cluster topology including namespaces, RBAC, and NetworkPolicies.

### API-key expiry and index coherence

`apikey-authz` watches `APIKey` resources and Secret metadata. For each namespace, it reads raw credentials only from the Secret named by `status.secretRef` and reads key identity and absolute expiry from `status.primaryKey` and `status.secondaryKey`. A credential is denied at the exact persisted deadline (`now >= expiresAt`); deletion of the Secret by the operator is cleanup rather than the authorization boundary.

A Secret is considered for a payload read only when its metadata has the exact annotation `kaito.sh/managed-by=llm-auth-service` and its controller owner matches the declared `APIKey`. The Secret informer is metadata-only; after the metadata gate passes, the full Secret is fetched through the uncached API reader and its annotation, owner, reference, lifecycle state, and status/data coherence are checked again before publication. The existing label `kaito.sh/managed-by=llm-auth-service` remains unchanged but is not an authorization gate. New Secrets receive both metadata fields, and the operator backfills the annotation on existing owned Secrets while preserving unrelated metadata. During upgrade, label-only Secrets fail closed until that reconciliation adds the annotation.

The metadata and payload GETs are not atomic. If metadata changes between them, the payload may reach the authz process, but the full-object checks prevent it from entering the index and the metadata-only informer never caches payloads. The join uses the existing eight-hex-character (32-bit) `keyID`, while the in-memory credential lookup remains keyed by the full SHA-256 hash. Duplicate or ambiguous IDs, an unexpected owner reference, and any present Secret key without a unique status match clear the whole namespace from the index. Secret-first lifecycle updates can therefore cause a brief denial while status catches up, but cannot retain a matched subset from mixed generations or allow a key past its deadline. Because the status join is only 32 bits, it is not a cryptographic binding to the Secret value; write access to the managed Secret is therefore trusted. No new CRD field is required.

## Getting Started

### Prerequisites

- Go 1.27+
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
