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
