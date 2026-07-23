# Daisy-chained ext_authz experiment

Validated in a local kind cluster (Istio 1.28 minimal, both `llm-gateway-apikey`
and `llm-gateway-azureauth` charts installed with `istio.enabled=false`) that a
single EnvoyFilter on the ingress gateway can fan out to two ext_authz backends
based on the shape of the token in `Authorization`:

- token begins with `sk-` → routed to **apikey-authz**
- anything else (JWT / missing) → routed to **azure-authz**

Adapted from
[production-stack/charts/modelharness/templates/envoyfilter-ext-authz.yaml](https://github.com/kaito-project/production-stack/blob/main/charts/modelharness/templates/envoyfilter-ext-authz.yaml).

## Filter chain

```
                   ┌──────────────────────────────────────────┐
Authorization ─►   │ envoy.filters.http.lua                   │
                   │   classify: sk-… → auth_mode=apikey       │
                   │   else       → auth_mode=azure            │
                   └──────────────────────────────────────────┘
                                     │
                                     ▼
                   ┌──────────────────────────────────────────┐
                   │ ext_authz.apikey  (filter_enabled_meta   │
                   │   auth_mode == "apikey")                  │
                   │   → apikey-authz.llm-gateway.svc:9001     │
                   └──────────────────────────────────────────┘
                                     │
                                     ▼
                   ┌──────────────────────────────────────────┐
                   │ envoy.filters.http.jwt_authn (Istio)      │
                   │   validates Microsoft signing keys, iss,  │
                   │   aud, exp — populates filter metadata    │
                   └──────────────────────────────────────────┘
                                     │
                                     ▼
                   ┌──────────────────────────────────────────┐
                   │ ext_authz.azure  (filter_enabled_meta    │
                   │   auth_mode == "azure",                  │
                   │   metadata_context_namespaces:            │
                   │     [envoy.filters.http.jwt_authn])       │
                   │   → azure-authz.llm-gateway.svc:9001      │
                   └──────────────────────────────────────────┘
```

`ext_authz.apikey` is inserted `INSERT_FIRST`; `ext_authz.azure` is inserted
`INSERT_AFTER jwt_authn` so the validated claims are in filter metadata before
azure-authz sees the request.

The two ext_authz filters share unique names (`ext_authz.apikey`,
`ext_authz.azure`) — not the canonical `envoy.filters.http.ext_authz` — so
`typed_per_filter_config` and dynamic-metadata gating can address them
independently.

## Validation matrix (kind, Istio 1.28)

| Request | Routed to | HTTP |
|---|---|---|
| `Bearer sk-…` (real APIKey CR in `tenant-a`) | apikey-authz | **200** allow |
| `Bearer sk-notreal` | apikey-authz | 401 deny |
| `Bearer <valid Microsoft JWT>` (aud=`management.core.windows.net`) | jwt_authn ✅ → azure-authz | reached SAR (`user=<oid>`) |
| `Bearer <expired JWT>` | jwt_authn | 401 (never reaches ext_authz) |
| `Bearer <bogus JWT>` | jwt_authn | 401 |
| no `Authorization` | azure-authz | 401 (no claims in filter metadata) |

## Files

- [`envoyfilter-ext-authz-daisy.yaml`](./envoyfilter-ext-authz-daisy.yaml) — the
  working filter as applied to `istio-system`.

## Prerequisites for the azure path to allow

1. **`RequestAuthentication`** on the gateway matching your tenant:

    ```yaml
    apiVersion: security.istio.io/v1beta1
    kind: RequestAuthentication
    metadata: {name: azure-jwt, namespace: istio-system}
    spec:
      selector: {matchLabels: {app: istio-ingressgateway}}
      jwtRules:
      - issuer: "https://sts.windows.net/<TENANT>/"
        audiences: ["<AUDIENCE>"]
        jwksUri: "https://login.microsoftonline.com/<TENANT>/discovery/keys"
        forwardOriginalToken: true
    ```

2. **A Host header the azure-authz namespace derivation accepts.** azure-authz
   parses the first DNS label of `:authority` as the target namespace (e.g.
   `tenant-a.svc.local` → `tenant-a`). Unlike apikey-authz it does **not** read
   `context_extensions[gateway-namespace]`, so per-virtual-host overrides don't
   help — worth patching upstream.

3. **RBAC that satisfies the SAR shape** azure-authz builds. Default
   `sar.resource=namespaces` is cluster-scoped, so a namespaced RoleBinding
   does not match. Either:
    - point `authz.sar.resource` at a namespaced type
      (e.g. `inferencepools.inference.networking.k8s.io`) and grant with
      `RoleBinding` in the tenant namespace, **or**
    - grant a `ClusterRoleBinding` for `inference` on `namespaces`.

## Known limitations of the current authz backends (not filter bugs)

- `apikey-authz`'s `extractKey` only strips exact `Bearer ` — lowercase
  `bearer` / uppercase `BEARER` / scheme-less tokens are correctly *routed*
  to apikey by the Lua classifier but then rejected by the backend.
- `azure-authz` ignores `context_extensions[gateway-namespace]` and requires
  a Host header whose first label is the namespace.
