# Cluster Resources Architecture

```
┌──────────────────────────────────────────────────────────────────────────┐
│              Inference Workloads (ns: inferencepool)                     │
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
│  ┌──────────────────────────┐                                            │
│  │ AuthorizationPolicy      │                                            │
│  │ inferencepool-deny-      │                                            │
│  │ unauthed                 │                                            │
│  │ → backstop: deny unauthed│                                            │
│  └──────────────────────────┘                                            │
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
│  │ extauthz                 │───▶│ extauthz                         │    │
│  │ → SAR authorization      │    │                                  │    │
│  │ → API key validation     │    │                                  │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
│                                                                          │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐    │
│  │ PeerAuthentication       │    │ ServiceAccount                   │    │
│  │ extauthz-strict-mtls     │    │ extauthz                         │    │
│  │ → mTLS between pods      │    │                                  │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
└──────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────┐
│              RBAC & Identity (cluster-scoped)                            │
│                                                                          │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐    │
│  │ ClusterRole              │    │ ClusterRole                      │    │
│  │ extauthz-sar             │    │ extauthz-inference               │    │
│  │ → create SAR             │    │ → "inference" on namespaces      │    │
│  └──────────────────────────┘    │   (apiGroup: aimanager)          │    │
│                                  └──────────────────────────────────┘    │
│  ┌──────────────────────────┐                                            │
│  │ ClusterRoleBinding       │                                            │
│  │ extauthz-sar             │                                            │
│  │ SA:extauthz →            │                                            │
│  │ CR:extauthz-sar          │                                            │
│  └──────────────────────────┘                                            │
└──────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────┐
│              Networking (ns: inferencepool)                               │
│                                                                          │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐    │
│  │ NetworkPolicy            │    │ NetworkPolicy                    │    │
│  │ default-deny-ingress     │    │ allow-inferencepool-traffic      │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
└──────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────┐
│              CRDs & Webhooks (cluster-scoped)                            │
│                                                                          │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐    │
│  │ CustomResourceDef        │    │ ValidatingWebhook                │    │
│  │ APIKey                   │    │ Configuration                    │    │
│  └──────────────────────────┘    └──────────────────────────────────┘    │
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
