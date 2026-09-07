// This file covers task 2.3: Traefik IngressRoute generation/deletion for a
// workspace's three externally reachable systems (design.md "Ingress
// Router", Requirement 11.1-11.7). No typed Go API for the Traefik CRD
// provider is vendored into this module, so these are built as
// unstructured.Unstructured, following the same approach as
// database_reconciler.go for CNPG.
package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	traefikAPIVersion = "traefik.io/v1alpha1"
	ingressRouteKind  = "IngressRoute"

	// forwardAuthChainMiddleware is apps/common/middlewares.yaml's chain. It
	// must be referenced by the CRD-provider-qualified name rather than the
	// middleware ref's own `namespace:` field: this cluster's Traefik does
	// not resolve that field for a cross-namespace Middleware (confirmed
	// against real router errors — see that file's own comment; the same
	// <namespace>-<name>@kubernetescrd form is already used by
	// apps/garage/values.yaml and apps/home-assistant/values.yaml).
	forwardAuthChainMiddleware = "argocd-forward-auth-chain@kubernetescrd"

	// wildcardCertSecret is cert-manager's *.fickledev.com certificate
	// (apps/cluster-issuer/wildcard-certificate.yaml), reflected into the
	// workspace namespace by that Certificate's secretTemplate annotations.
	wildcardCertSecret = "tls-fickledev-com"

	ingressEntryPoint = "websecure"

	// fickledevDomain is the zone the wildcard cert and cloudflared tunnel
	// (my-home-network terraform/cloudflare_dns.tf, cloudflare_zero_trust.tf)
	// cover.
	fickledevDomain = "fickledev.com"

	// Backend ports the per-workspace Service is expected to expose once the
	// Test Runner (task 9) exists to back them; nothing creates that Service
	// yet. Declaring the contract here lets that task land without
	// renegotiating the routing config.
	previewServicePort = 3000
	reportServicePort  = 8080

	hostSuffixPreview = "-preview"
	hostSuffixReport  = "-report"
)

// workspaceHostnames are the three single-label hostnames a workspace is
// reachable at. Requirement 11.2: the wildcard cert's SAN (*.fickledev.com)
// matches exactly one label, so every hostname here must be a bare label
// with no further dots — a multi-level hostname TLS-terminates silently with
// Traefik's default self-signed certificate instead of failing loudly. The
// three systems are told apart by a suffix on that label, not by depth.
// Session is not routed from here: it is served by the single shared Terminal
// Gateway, whose wildcard route (apps/devplatform/templates/gateway-ingressroute.yaml)
// catches the bare label and resolves it back to this Workspace through the
// Kubernetes API. A per-workspace route for it would outrank that wildcard —
// Traefik prioritises by rule length — and point the hostname at a backend
// that does not exist.
type workspaceHostnames struct {
	Preview string
	Session string
	Report  string
}

func deriveWorkspaceHostnames(resourceName string) workspaceHostnames {
	return workspaceHostnames{
		Preview: hostLabelWithSuffix(resourceName, hostSuffixPreview),
		Session: resourceName,
		Report:  hostLabelWithSuffix(resourceName, hostSuffixReport),
	}
}

// hostLabelWithSuffix truncates base so base+suffix still fits a DNS-1123
// label (naming.go already caps resourceName at 63 chars for the bare/Session
// case; the Preview/Report variants need headroom left for their suffix).
//
// ponytail: truncating a shared long base for two different suffixes can, in
// principle, make two distinct resourceNames collide on the same
// preview/report hostname if they agree on the first (63-len(suffix))
// characters — DeriveResourceName's collision hash only protects the
// untruncated resourceName. Real branch-derived resourceNames are far short
// of 63 chars in practice; if this ever bites, derive host labels from a
// fixed-width hash of resourceName instead of a truncated prefix.
func hostLabelWithSuffix(base, suffix string) string {
	return truncateDNSLabel(base, dnsLabelMaxLength-len(suffix)) + suffix
}

func ingressRouteRef(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(traefikAPIVersion)
	u.SetKind(ingressRouteKind)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

// buildIngressRoute wires host -> forward-auth-chain -> resourceName's
// (future) Service:port. entryPoints is restricted to websecure: these
// hostnames only exist behind the wildcard cert, and cloudflared's tunnel
// config (my-home-network) forwards its ingress to Traefik over HTTPS.
func buildIngressRoute(ws *devplatformv1alpha1.Workspace, name, host, resourceName string, port int64) *unstructured.Unstructured {
	ir := ingressRouteRef(ws.Namespace, name)
	ir.SetLabels(workspaceLabels(ws))
	ir.Object["spec"] = map[string]interface{}{
		"entryPoints": []interface{}{ingressEntryPoint},
		"routes": []interface{}{
			map[string]interface{}{
				"kind":  "Rule",
				"match": "Host(`" + host + "." + fickledevDomain + "`)",
				"middlewares": []interface{}{
					map[string]interface{}{"name": forwardAuthChainMiddleware},
				},
				"services": []interface{}{
					map[string]interface{}{
						"name": resourceName,
						"port": port,
					},
				},
			},
		},
		"tls": map[string]interface{}{
			"secretName": wildcardCertSecret,
		},
	}
	return ir
}

// reconcileIngress creates the IngressRoutes fronting a workspace and returns
// all three URLs it makes reachable (11.1). Each route carries an
// OwnerReference to ws, so Kubernetes' garbage collector removes it only
// when ws itself is deleted (11.7). No cloudflared/DNS change is needed per
// workspace: every hostname here already falls under the wildcard already
// tunnelled to Traefik (11.3/11.4).
func (r *WorkspaceReconciler) reconcileIngress(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName string) (devplatformv1alpha1.WorkspaceURLs, error) {
	hosts := deriveWorkspaceHostnames(resourceName)

	routes := [...]struct {
		name string
		host string
		port int64
	}{
		{resourceName + hostSuffixPreview, hosts.Preview, previewServicePort},
		{resourceName + hostSuffixReport, hosts.Report, reportServicePort},
	}
	for _, rt := range routes {
		ir := buildIngressRoute(ws, rt.name, rt.host, resourceName, rt.port)
		if err := controllerutil.SetControllerReference(ws, ir, r.Scheme); err != nil {
			return devplatformv1alpha1.WorkspaceURLs{}, err
		}
		if err := r.ensureCreated(ctx, ir); err != nil {
			return devplatformv1alpha1.WorkspaceURLs{}, err
		}
	}

	return devplatformv1alpha1.WorkspaceURLs{
		Preview: "https://" + hosts.Preview + "." + fickledevDomain,
		Session: "https://" + hosts.Session + "." + fickledevDomain,
		Report:  "https://" + hosts.Report + "." + fickledevDomain,
	}, nil
}
