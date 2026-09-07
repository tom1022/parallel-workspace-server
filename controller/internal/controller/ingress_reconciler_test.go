package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

func getIngressRoute(t *testing.T, ctx context.Context, ns, name string) *unstructured.Unstructured {
	t.Helper()
	ir := ingressRouteRef(ns, name)
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, ir); err != nil {
		t.Fatalf("get IngressRoute %s: %v", name, err)
	}
	return ir
}

// provisionToReady drives a Workspace through Reconcile until the
// StatefulSet is marked ready and one more pass runs reconcileIngress, and
// returns the derived resourceName.
func provisionToReady(t *testing.T, ctx context.Context, ns, name, branch string) string {
	t.Helper()
	createWorkspace(t, ctx, ns, name, "https://gitea.fickledev.com/tom1022/demo.git", branch, "default")

	r := newTestReconciler()
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

func TestReconcile_CreatesPreviewAndReportIngressRoutes(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-ingress", "feature/ingress")

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

		if len(ir.GetOwnerReferences()) != 1 || ir.GetOwnerReferences()[0].Name != "ws-ingress" {
			t.Errorf("IngressRoute %s ownerReferences = %+v, want a single reference to ws-ingress", name, ir.GetOwnerReferences())
		}

		routes, _, _ := unstructured.NestedSlice(ir.Object, "spec", "routes")
		if len(routes) != 1 {
			t.Fatalf("IngressRoute %s spec.routes has %d entries, want 1", name, len(routes))
		}
		route, _ := routes[0].(map[string]interface{})

		wantMatch := "Host(`" + c.wantHost + ".fickledev.com`)"
		if got, _, _ := unstructured.NestedString(route, "match"); got != wantMatch {
			t.Errorf("IngressRoute %s spec.routes[0].match = %q, want %q", name, got, wantMatch)
		}

		middlewares, _, _ := unstructured.NestedSlice(route, "middlewares")
		if len(middlewares) != 1 {
			t.Fatalf("IngressRoute %s spec.routes[0].middlewares has %d entries, want 1", name, len(middlewares))
		}
		mw, _ := middlewares[0].(map[string]interface{})
		if got, _, _ := unstructured.NestedString(mw, "name"); got != forwardAuthChainMiddleware {
			t.Errorf("IngressRoute %s middleware name = %q, want %q", name, got, forwardAuthChainMiddleware)
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

		if got, _, _ := unstructured.NestedString(ir.Object, "spec", "tls", "secretName"); got != wildcardCertSecret {
			t.Errorf("IngressRoute %s spec.tls.secretName = %q, want %q", name, got, wildcardCertSecret)
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

	wantSession := "https://" + resourceName + ".fickledev.com"
	wantPreview := "https://" + resourceName + "-preview.fickledev.com"
	wantReport := "https://" + resourceName + "-report.fickledev.com"

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
			t.Errorf("hostname %q must be a single label (no dots) to match the *.fickledev.com wildcard SAN", h)
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
	// IngressRoute carries ws as its controller owner (asserted already in
	// TestReconcile_CreatesPreviewAndReportIngressRoutes), which is what makes
	// Kubernetes delete it when the Workspace is deleted. This test instead
	// asserts the owner reference points at the live Workspace UID so a
	// stale/zero UID (which would silently break GC) is caught.
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
	ir := ingressRouteRef(ns, name)
	err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, ir)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get IngressRoute %s error = %v, want NotFound", name, err)
	}
}
