// This file covers task 3.1: making a workspace's preview and report
// endpoints reachable through a pluggable RoutingAdapter (design.md "Routing
// Adapter", Requirement 3.1/3.2, internal/adapter/routing) instead of a
// hardcoded Traefik IngressRoute. The reconcile loop asks the adapter for
// "a reachable entry point" and never learns which resource kind backs it.
package controller

import (
	"context"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/adapter/routing"
)

const (
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
// reachable at. Requirement 11.2: the public certificate's SAN matches
// exactly one label, so every hostname here must be a bare label with no
// further dots — a multi-level hostname TLS-terminates silently with the
// ingress implementation's own default certificate instead of failing
// loudly. The three systems are told apart by a suffix on that label, not by
// depth. Session is not routed through RoutingAdapter: it is served by the
// single shared Terminal Gateway, whose own wildcard route
// (apps/devplatform/templates/gateway-ingressroute.yaml) catches the bare
// label and resolves it back to this Workspace through the Kubernetes API. A
// per-workspace route for it would outrank that wildcard — Traefik
// prioritises by rule length — and point the hostname at a backend that does
// not exist.
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

// routingTarget builds the RoutingAdapter request for ws/resourceName.
// Remove only reads Namespace/ServiceName (see RoutingTarget), so callers
// that only need to identify already-generated resources for cleanup
// (failAndRollback) can use this too, not just Ensure.
func (r *WorkspaceReconciler) routingTarget(ws *devplatformv1alpha1.Workspace, resourceName string) routing.RoutingTarget {
	hosts := deriveWorkspaceHostnames(resourceName)
	return routing.RoutingTarget{
		WorkspaceName: ws.Name,
		WorkspaceUID:  ws.UID,
		Namespace:     ws.Namespace,
		ServiceName:   resourceName,
		Hostnames: routing.RoutingHostnames{
			Preview: hosts.Preview,
			Report:  hosts.Report,
		},
		Ports: routing.RoutingPorts{
			Preview: previewServicePort,
			Report:  reportServicePort,
		},
		Labels:   workspaceLabels(ws),
		Exposure: r.RoutingExposure,
	}
}

// reconcileIngress makes the workspace's preview and report endpoints
// reachable through whichever RoutingAdapter this deployment selected, and
// returns all three URLs the workspace is reachable at (11.1). Session needs
// no per-workspace resource (see workspaceHostnames), so its URL is built
// directly from r.Domain instead of round-tripping through the adapter.
func (r *WorkspaceReconciler) reconcileIngress(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName string) (devplatformv1alpha1.WorkspaceURLs, error) {
	hosts := deriveWorkspaceHostnames(resourceName)

	if err := r.RoutingAdapter.Ensure(ctx, r.routingTarget(ws, resourceName)); err != nil {
		return devplatformv1alpha1.WorkspaceURLs{}, err
	}

	return devplatformv1alpha1.WorkspaceURLs{
		Preview: "https://" + hosts.Preview + "." + r.Domain,
		Session: "https://" + hosts.Session + "." + r.Domain,
		Report:  "https://" + hosts.Report + "." + r.Domain,
	}, nil
}
