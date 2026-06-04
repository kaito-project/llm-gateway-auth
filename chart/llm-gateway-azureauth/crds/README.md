# Istio CRDs (temporary)

This directory ships Istio CRDs bundled with the `llm-gateway-azureauth`
chart so that the `EnvoyFilter` (networking.istio.io/v1alpha3),
`PeerAuthentication` (security.istio.io/v1), and `RequestAuthentication`
(security.istio.io/v1) templates under
`chart/llm-gateway-azureauth/templates/istio/` can apply cleanly on AKS
clusters that use the **Azure AKS application routing add-on**.

## Why is this here?

The AKS app routing add-on installs and manages Istio under the hood,
but it does **not** currently install the full set of Istio CRDs that
arbitrary user workloads may need — in particular, `EnvoyFilter`,
`PeerAuthentication`, and `RequestAuthentication` are not always
present out of the box. Without these CRDs, `helm install` of
`llm-gateway-azureauth` fails because the chart's templates reference Kinds
the API server does not know about.

To keep the chart self-contained and installable on app-routing-enabled
clusters, we vendor the CRDs here. Helm installs files under `crds/`
before any templates and never upgrades or deletes them, so this is
safe to ship alongside a managed Istio control plane.

## When can this be removed?

Delete this directory once the Azure AKS application routing add-on
installs the required Istio CRDs (`EnvoyFilter`, `PeerAuthentication`,
`RequestAuthentication`) by default. At that point the managed control
plane will own the CRD lifecycle and shipping our own copies risks
version drift.

## Source

CRDs were extracted verbatim from upstream Istio's
`manifests/charts/base/files/crd-all.gen.yaml` (release-1.28). The
upstream `helm.sh/resource-policy: keep` annotation is preserved so
that uninstalling `llm-gateway-azureauth` does not remove CRDs that other
workloads may rely on.
