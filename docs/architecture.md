# Cluster Resources Architecture

```
┌──────────────────────────────────────────────────────────────────────────┐
│              Inference Workloads (ns: inferencepool-A)                   │
│                                                                          │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐    │
│  │ GatewayClass             │    │ Gateway                          │    │
│  │ inference-gateway-class  │───▶│ inferencepool-gateway            │    │
│  │ (cluster-scoped)         │    │                                  │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
│                                        │                                 │
│  ┌──────────────────────────┐          ▼                                 │
│  │ HTTPRoute                │    ┌──────────────────────────────────┐    │
│  │ inferencepool-route      │───▶│ InferencePool                    │    │
│  └──────────────────────────┘    │ inferencepool                    │    │
│                                  └──────────────────────────────────┘    │
│                                        │                                 │
│  ┌──────────────────────────┐          ▼                                 │
│  │ Endpoint Picker (EPP)    │    ┌──────────────────────────────────┐    │
│  │ → model-aware routing    │    │ InferenceModel                   │    │
│  │ → real-time metrics      │    │ (model-aware routing)            │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
│                                                                          │
│  ┌ AuthN/AuthZ ──────────────────────────────────────────────────────┐   │
│  │                                                                   │   │
│  │  ┌──────────────────────────┐  ┌──────────────────────────────┐   │   │
│  │  │ AuthorizationPolicy      │  │ APIKey (CR)                  │   │   │
│  │  │ inferencepool-deny-      │  │ → API key resources for      │   │   │
│  │  │ unauthed                 │  │   this inference pool        │   │   │
│  │  │ → backstop: deny unauthed│  │                              │   │   │
│  │  └──────────────────────────┘  └──────────────────────────────┘   │   │
│  └───────────────────────────────────────────────────────────────────┘   │
│                                                                          │
│  ┌ Network Policies ─────────────────────────────────────────────────┐   │
│  │                                                                   │   │
│  │  ┌──────────────────────────┐  ┌──────────────────────────────┐   │   │
│  │  │ NetworkPolicy            │  │ NetworkPolicy                │   │   │
│  │  │ default-deny-ingress     │  │ allow-inferencepool-traffic  │   │   │
│  │  └──────────────────────────┘  └──────────────────────────────┘   │   │
│  └───────────────────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────┐
│              Istio Auth (ns: aks-istio-ingress)                          │
│                                                                          │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐    │
│  │ RequestAuthentication    │    │ AuthorizationPolicy              │    │
│  │ azure-ad-jwt             │    │ gateway-ext-authz                │    │
│  │ → JWT validation         │    │ → forwards to extauthz svc       │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
│                                                                          │
│  ┌──────────────────────────┐                                            │
│  │ EnvoyFilter              │                                            │
│  │ reorder-jwt-before-      │                                            │
│  │ extauthz                 │                                            │
│  │ → ensure JWT validated   │                                            │
│  │   before ext-authz check │                                            │
│  └──────────────────────────┘                                            │
└──────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────┐
│              Ext-Authz Workload (ns: llm-gateway)                        │
│                                                                          │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐    │
│  │ Deployment               │    │ Service                          │    │
│  │ extauthz-azure           │───▶│ extauthz-azure                   │    │
│  │ → SAR authorization      │    │                                  │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
│                                                                          │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐    │
│  │ Deployment               │    │ Service                          │    │
│  │ extauthz-apikey          │───▶│ extauthz-apikey                  │    │
│  │ → API key validation     │    │                                  │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
│                                                                          │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐    │
│  │ Deployment               │    │ ServiceAccount                   │    │
│  │ apikey-controller        │    │ apikey-controller                │    │
│  │ → watches APIKey CRs     │    │                                  │    │
│  │ → create, rotate, expire │    │                                  │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
│                                                                          │
│  ┌──────────────────────────┐                                            │
│  │ PeerAuthentication       │                                            │
│  │ extauthz-strict-mtls     │                                            │
│  │ → mTLS between pods      │                                            │
│  │ (optional: requires      │                                            │
│  │  sidecar injection)      │                                            │
│  └──────────────────────────┘                                            │
└──────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────┐
│              Cluster-Scoped Resources                                    │
│                                                                          │
│  ┌ RBAC & Identity ─────────────────────────────────────────────────┐    │
│  │                                                                  │    │
│  │  ┌──────────────────────────┐  ┌──────────────────────────────┐  │    │
│  │  │ ClusterRole              │  │ ClusterRole                  │  │    │
│  │  │ extauthz-sar             │  │ extauthz-inference           │  │    │
│  │  │ → create SAR             │  │ → "inference" on namespaces  │  │    │
│  │  └──────────────────────────┘  │   (apiGroup: aimanager)      │  │    │
│  │                                └──────────────────────────────┘  │    │
│  │  ┌──────────────────────────┐                                    │    │
│  │  │ ClusterRoleBinding       │                                    │    │
│  │  │ extauthz-sar             │                                    │    │
│  │  │ SA:extauthz →            │                                    │    │
│  │  │ CR:extauthz-sar          │                                    │    │
│  │  └──────────────────────────┘                                    │    │
│  └──────────────────────────────────────────────────────────────────┘    │
│                                                                          │
│  ┌ CRDs & Webhooks ─────────────────────────────────────────────────┐    │
│  │                                                                  │    │
│  │  ┌──────────────────────────┐  ┌──────────────────────────────┐  │    │
│  │  │ CustomResourceDef        │  │ ValidatingWebhook            │  │    │
│  │  │ APIKey                   │  │ Configuration                │  │    │
│  │  └──────────────────────────┘  └──────────────────────────────┘  │    │
│  └──────────────────────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────────────────────┘

Flow:
  [ns: aks-istio-ingress]
  Ingress → Gateway → RequestAuthentication (JWT)
                     → EnvoyFilter (reorder)
                     → AuthorizationPolicy → extauthz (ns: llm-gateway)
                                                │
                                          SAR + API key check
                                                │
                                                ▼
  [ns: inferencepool]
  HTTPRoute → InferencePool → EPP → InferenceModel → Model endpoint
```

## API-key authorization data flow

The API-key operator owns the plaintext Secret and publishes each generation's identity and absolute deadline in `APIKey.status`. New Secrets receive the exact annotation `kaito.sh/managed-by=llm-auth-service` and retain the existing label `kaito.sh/managed-by=llm-auth-service`. Existing owned Secrets have the annotation backfilled during reconciliation without replacing unrelated labels or annotations. The label remains unchanged but is not the read gate; a label-only Secret fails closed until the operator adds the annotation.

The authz `IndexSyncer` watches APIKey updates and Secret metadata. It fetches `APIKey/default`, then reads cached `PartialObjectMetadata` for only the Secret named by `status.secretRef`. The metadata must have the exact managed annotation and an APIKey controller owner whose name and UID match the APIKey before the syncer performs a payload read. Secret watches are registered with `WatchesMetadata`, so payloads are not stored in the shared informer cache.

After the metadata gate passes, the syncer fetches the full Secret only through the uncached API reader. It revalidates the annotation and owner, then checks the declared reference, deletion state, lifecycle status, and status/data coherence before publication. The metadata and payload GETs are not atomic: if metadata changes between them, the payload may reach the authz process, but the full-object validation prevents that snapshot from entering the authorization index.

Each present `apiKey` or `secondaryApiKey` value is SHA-256 hashed and matched across both primary and secondary status entries by the existing eight-hex-character `keyID`; physical Secret slots do not determine status identity. The full hash—not the short ID—is the in-memory lookup key. Duplicate or ambiguous IDs and any unmatched present credential invalidate the complete namespace replacement. An unmatched secondary status is permitted only so Secret-first grace-period cleanup remains available while status catches up. The 32-bit join is not a cryptographic binding to the Secret value, so principals with write access to the managed Secret are inside the trust boundary.

The complete namespace map is published atomically only when the resources are coherent. Creation, rotation, metadata migration, and cleanup may therefore have brief fail-closed windows between Secret and status writes. Request handling independently denies finite keys when `now >= expiresAt`, so operator reconciliation and Secret deletion are not on the expiry enforcement path. The process currently reports serving independently of initial namespace index synchronization; defining readiness across isolated namespaces and migration states is intentionally separate from this read protocol.
