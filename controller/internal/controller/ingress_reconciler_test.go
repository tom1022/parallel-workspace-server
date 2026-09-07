package controller

import (
	"context"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/adapter/routing"
)

// testRoutingDomain/testRoutingTLSSecretName are the fixture values
// newTestReconciler's default (Traefik) RoutingAdapter uses, matching what
// this deployment ran before RoutingAdapter existed (task 3.1) so tests
// unrelated to routing don't need their own URL/domain assertions changed.
// Tests about routing adapter selection itself use their own, different
// fixture values instead, precisely to prove those values come from
// configuration rather than being hardcoded in the implementation.
const (
	testRoutingDomain        = "fickledev.com"
	testRoutingTLSSecretName = "tls-fickledev-com"
)

// ingressRouteRef is a test-only helper (also used by
// integration_provision_test.go): it just forwards to the routing package's
// exported ref constructor so tests can Get/assert-absent a generated
// Traefik IngressRoute without this package needing to know the object's
// shape itself.
func ingressRouteRef(ns, name string) *unstructured.Unstructured {
	return routing.TraefikIngressRouteRef(ns, name)
}

func getIngressRoute(t *testing.T, ctx context.Context, ns, name string) *unstructured.Unstructured {
	t.Helper()
	ir := ingressRouteRef(ns, name)
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, ir); err != nil {
		t.Fatalf("get IngressRoute %s: %v", name, err)
	}
	return ir
}

func getIngress(t *testing.T, ctx context.Context, ns, name string) *networkingv1.Ingress {
	t.Helper()
	var ing networkingv1.Ingress
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &ing); err != nil {
		t.Fatalf("get Ingress %s: %v", name, err)
	}
	return &ing
}

// provisionToReady drives a Workspace through Reconcile until the
// StatefulSet is marked ready and one more pass runs reconcileIngress, and
// returns the derived resourceName.
func provisionToReady(t *testing.T, ctx context.Context, ns, name, branch string) string {
	t.Helper()
	return provisionToReadyWithReconciler(t, ctx, newTestReconciler(), ns, name, branch)
}

// provisionToReadyWithReconciler is provisionToReady with an explicit
// reconciler, so tests exercising a specific RoutingAdapter configuration
// don't have to duplicate the drive-to-Ready dance.
func provisionToReadyWithReconciler(t *testing.T, ctx context.Context, r *WorkspaceReconciler, ns, name, branch string) string {
	t.Helper()
	createWorkspace(t, ctx, ns, name, "https://gitea.fickledev.com/tom1022/demo.git", branch, "default")

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req.NamespacedName, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	resourceName := ws.Status.WorkspaceId
	markStatefulSetReady(t, ctx, ns, resourceName)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	return resourceName
}

// TestReconcile_TraefikAdapter_CreatesPreviewAndReportIngressRoutes proves
// the Traefik RoutingAdapter generates one IngressRoute per entry point,
// wired from its own configuration (a domain/secret distinct from
// testRoutingDomain/testRoutingTLSSecretName, which would otherwise mask a
// hardcoded literal) rather than from a literal in the implementation
// (Requirement 3.2).
func TestReconcile_TraefikAdapter_CreatesPreviewAndReportIngressRoutes(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")

	const (
		domain  = "traefik-example.test"
		tlsName = "traefik-example-tls"
	)
	r := newTestReconciler()
	r.RoutingAdapter = &routing.TraefikAdapter{
		Client:        testClient,
		Scheme:        scheme.Scheme,
		Domain:        domain,
		TLSSecretName: tlsName,
	}
	r.Domain = domain

	resourceName := provisionToReadyWithReconciler(t, ctx, r, ns, "ws-ingress-traefik", "feature/ingress-traefik")

	cases := []struct {
		routeSuffix string
		wantHost    string
		wantPort    int64
	}{
		{"-preview", resourceName + "-preview", previewServicePort},
		{"-report", resourceName + "-report", reportServicePort},
	}

	for _, c := range cases {
		name := resourceName + c.routeSuffix
		ir := getIngressRoute(t, ctx, ns, name)

		if len(ir.GetOwnerReferences()) != 1 || ir.GetOwnerReferences()[0].Name != "ws-ingress-traefik" {
			t.Errorf("IngressRoute %s ownerReferences = %+v, want a single reference to ws-ingress-traefik", name, ir.GetOwnerReferences())
		}

		routes, _, _ := unstructured.NestedSlice(ir.Object, "spec", "routes")
		if len(routes) != 1 {
			t.Fatalf("IngressRoute %s spec.routes has %d entries, want 1", name, len(routes))
		}
		route, _ := routes[0].(map[string]interface{})

		wantMatch := "Host(`" + c.wantHost + "." + domain + "`)"
		if got, _, _ := unstructured.NestedString(route, "match"); got != wantMatch {
			t.Errorf("IngressRoute %s spec.routes[0].match = %q, want %q", name, got, wantMatch)
		}

		if _, ok, _ := unstructured.NestedSlice(route, "middlewares"); ok {
			t.Errorf("IngressRoute %s spec.routes[0].middlewares present with no Exposure.MiddlewareRefs configured, want absent", name)
		}

		services, _, _ := unstructured.NestedSlice(route, "services")
		if len(services) != 1 {
			t.Fatalf("IngressRoute %s spec.routes[0].services has %d entries, want 1", name, len(services))
		}
		svc, _ := services[0].(map[string]interface{})
		if got, _, _ := unstructured.NestedString(svc, "name"); got != resourceName {
			t.Errorf("IngressRoute %s service name = %q, want %q", name, got, resourceName)
		}
		if got, ok, _ := unstructured.NestedInt64(svc, "port"); !ok || got != c.wantPort {
			t.Errorf("IngressRoute %s service port = %v (found=%v), want %d", name, got, ok, c.wantPort)
		}

		if got, _, _ := unstructured.NestedString(ir.Object, "spec", "tls", "secretName"); got != tlsName {
			t.Errorf("IngressRoute %s spec.tls.secretName = %q, want %q", name, got, tlsName)
		}
	}
}

// TestReconcile_IngressAdapter_CreatesPreviewAndReportIngresses proves the
// standard IngressAdapter generates one Ingress per entry point from its own
// configuration, with no product-specific CRD involved (Requirement 3.1).
func TestReconcile_IngressAdapter_CreatesPreviewAndReportIngresses(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")

	const (
		domain    = "ingress-example.test"
		tlsName   = "ingress-example-tls"
		className = "nginx"
	)
	r := newTestReconciler()
	r.RoutingAdapter = &routing.IngressAdapter{
		Client:        testClient,
		Scheme:        scheme.Scheme,
		Domain:        domain,
		TLSSecretName: tlsName,
		ClassName:     className,
	}
	r.Domain = domain

	resourceName := provisionToReadyWithReconciler(t, ctx, r, ns, "ws-ingress-std", "feature/ingress-std")

	cases := []struct {
		suffix   string
		wantHost string
		wantPort int32
	}{
		{"-preview", resourceName + "-preview", previewServicePort},
		{"-report", resourceName + "-report", reportServicePort},
	}

	for _, c := range cases {
		name := resourceName + c.suffix
		ing := getIngress(t, ctx, ns, name)

		if len(ing.OwnerReferences) != 1 || ing.OwnerReferences[0].Name != "ws-ingress-std" {
			t.Errorf("Ingress %s ownerReferences = %+v, want a single reference to ws-ingress-std", name, ing.OwnerReferences)
		}

		if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != className {
			t.Errorf("Ingress %s IngressClassName = %v, want %q", name, ing.Spec.IngressClassName, className)
		}

		wantFQDN := c.wantHost + "." + domain
		if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != wantFQDN {
			t.Fatalf("Ingress %s Rules = %+v, want a single rule for host %q", name, ing.Spec.Rules, wantFQDN)
		}
		rule := ing.Spec.Rules[0]
		if rule.HTTP == nil || len(rule.HTTP.Paths) != 1 {
			t.Fatalf("Ingress %s HTTP paths = %+v, want exactly 1", name, rule.HTTP)
		}
		backend := rule.HTTP.Paths[0].Backend
		if backend.Service == nil || backend.Service.Name != resourceName || backend.Service.Port.Number != c.wantPort {
			t.Errorf("Ingress %s backend = %+v, want service %q port %d", name, backend, resourceName, c.wantPort)
		}

		if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != tlsName || len(ing.Spec.TLS[0].Hosts) != 1 || ing.Spec.TLS[0].Hosts[0] != wantFQDN {
			t.Errorf("Ingress %s TLS = %+v, want secretName %q for host %q", name, ing.Spec.TLS, tlsName, wantFQDN)
		}
	}
}

// TestReconcile_SelectingIngressDoesNotCreateIngressRoute and its Traefik
// counterpart below lock in Requirement 3.2: choosing one routing
// implementation must not also generate the other's resource kind.
func TestReconcile_SelectingIngressDoesNotCreateIngressRoute(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")

	r := newTestReconciler()
	r.RoutingAdapter = &routing.IngressAdapter{
		Client:        testClient,
		Scheme:        scheme.Scheme,
		Domain:        testRoutingDomain,
		TLSSecretName: testRoutingTLSSecretName,
	}

	resourceName := provisionToReadyWithReconciler(t, ctx, r, ns, "ws-ingress-only", "feature/ingress-only")

	for _, suffix := range []string{hostSuffixPreview, hostSuffixReport} {
		name := resourceName + suffix
		err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, ingressRouteRef(ns, name))
		if !apierrors.IsNotFound(err) {
			t.Errorf("get IngressRoute %s error = %v, want NotFound (IngressAdapter must not create Traefik resources)", name, err)
		}
	}
}

func TestReconcile_SelectingTraefikDoesNotCreateIngress(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")

	r := newTestReconciler() // default RoutingAdapter is already Traefik

	resourceName := provisionToReadyWithReconciler(t, ctx, r, ns, "ws-traefik-only", "feature/traefik-only")

	for _, suffix := range []string{hostSuffixPreview, hostSuffixReport} {
		name := resourceName + suffix
		err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &networkingv1.Ingress{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("get Ingress %s error = %v, want NotFound (TraefikAdapter must not create standard Ingress resources)", name, err)
		}
	}
}

// TestReconcile_RoutingExposureAppliedToPreviewAndReport locks in the
// exposure hook (design.md "RoutingExposure"): operator-configured
// annotations and middleware references land verbatim on both entry points,
// with the platform neither interpreting nor requiring them.
func TestReconcile_RoutingExposureAppliedToPreviewAndReport(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")

	r := newTestReconciler()
	r.RoutingExposure = routing.RoutingExposure{
		Annotations:    map[string]string{"cloudflared.example/tunnel": "preview-tunnel"},
		MiddlewareRefs: []string{"argocd-forward-auth-chain@kubernetescrd"},
	}

	resourceName := provisionToReadyWithReconciler(t, ctx, r, ns, "ws-exposure", "feature/exposure")

	for _, suffix := range []string{hostSuffixPreview, hostSuffixReport} {
		name := resourceName + suffix
		ir := getIngressRoute(t, ctx, ns, name)

		if got := ir.GetAnnotations()["cloudflared.example/tunnel"]; got != "preview-tunnel" {
			t.Errorf("IngressRoute %s annotation = %q, want %q", name, got, "preview-tunnel")
		}

		routes, _, _ := unstructured.NestedSlice(ir.Object, "spec", "routes")
		route, _ := routes[0].(map[string]interface{})
		middlewares, _, _ := unstructured.NestedSlice(route, "middlewares")
		if len(middlewares) != 1 {
			t.Fatalf("IngressRoute %s middlewares = %v, want 1 entry", name, middlewares)
		}
		mw, _ := middlewares[0].(map[string]interface{})
		if got, _, _ := unstructured.NestedString(mw, "name"); got != "argocd-forward-auth-chain@kubernetescrd" {
			t.Errorf("IngressRoute %s middleware name = %q, want %q", name, got, "argocd-forward-auth-chain@kubernetescrd")
		}
	}
}

// TestReconcile_SetsThreeWorkspaceURLs locks in 1.7/11.1: by the time a
// Workspace reaches Ready, status.urls carries all three externally
// reachable endpoints, not just the session URL.
func TestReconcile_SetsThreeWorkspaceURLs(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-urls", "feature/urls")

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, types.NamespacedName{Name: "ws-urls", Namespace: ns}, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}

	wantSession := "https://" + resourceName + "." + testRoutingDomain
	wantPreview := "https://" + resourceName + "-preview." + testRoutingDomain
	wantReport := "https://" + resourceName + "-report." + testRoutingDomain

	if ws.Status.Urls.Session != wantSession {
		t.Errorf("Urls.Session = %q, want %q", ws.Status.Urls.Session, wantSession)
	}
	if ws.Status.Urls.Preview != wantPreview {
		t.Errorf("Urls.Preview = %q, want %q", ws.Status.Urls.Preview, wantPreview)
	}
	if ws.Status.Urls.Report != wantReport {
		t.Errorf("Urls.Report = %q, want %q", ws.Status.Urls.Report, wantReport)
	}
}

func TestDeriveWorkspaceHostnames_SingleLabelNoDots(t *testing.T) {
	hosts := deriveWorkspaceHostnames("feature-foo")
	for _, h := range []string{hosts.Preview, hosts.Session, hosts.Report} {
		if strings.Contains(h, ".") {
			t.Errorf("hostname %q must be a single label (no dots) to match the wildcard certificate's SAN", h)
		}
	}
	if hosts.Preview == hosts.Session || hosts.Session == hosts.Report || hosts.Preview == hosts.Report {
		t.Errorf("the three hostnames must be distinct: %+v", hosts)
	}
}

func TestDeriveWorkspaceHostnames_LongResourceNameStaysWithinDNSLabel(t *testing.T) {
	long := strings.Repeat("a", 63) // the max a resourceName can already be
	hosts := deriveWorkspaceHostnames(long)
	for _, h := range []string{hosts.Preview, hosts.Session, hosts.Report} {
		if len(h) > dnsLabelMaxLength {
			t.Errorf("hostname %q (%d chars) exceeds the %d-char DNS label limit", h, len(h), dnsLabelMaxLength)
		}
		if strings.HasSuffix(h, "-") {
			t.Errorf("hostname %q must not end with '-' after truncation", h)
		}
	}
}

func TestReconcile_DestroyGCsIngressRoutes(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-ingress-gc", "feature/ingress-gc")

	// envtest has no kube-controller-manager to actually run garbage
	// collection, so this only locks in the precondition GC depends on: every
	// generated entry point carries ws as its controller owner (asserted
	// already in the *_CreatesPreviewAndReport* tests above), which is what
	// makes Kubernetes delete it when the Workspace is deleted. This test
	// instead asserts the owner reference points at the live Workspace UID
	// so a stale/zero UID (which would silently break GC) is caught.
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, types.NamespacedName{Name: "ws-ingress-gc", Namespace: ns}, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}

	ir := getIngressRoute(t, ctx, ns, resourceName+hostSuffixPreview)
	owner := ir.GetOwnerReferences()[0]
	if owner.UID != ws.UID {
		t.Errorf("IngressRoute owner UID = %q, want Workspace UID %q", owner.UID, ws.UID)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Error("IngressRoute owner reference is not marked as controller; GC-on-delete relies on this")
	}
}

// TestReconcile_LeavesSessionHostnameToTheGateway locks in 4.3/4.9: the
// connection hostname is served by the single shared Terminal Gateway, which
// resolves it back to this Workspace through the Kubernetes API. A
// per-workspace route here would take precedence over the gateway's wildcard
// route and strand the hostname on a backend that does not exist.
func TestReconcile_LeavesSessionHostnameToTheGateway(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-ingress-session", "feature/ingress-session")

	name := resourceName + "-session"
	err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, ingressRouteRef(ns, name))
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get IngressRoute %s error = %v, want NotFound", name, err)
	}
}
