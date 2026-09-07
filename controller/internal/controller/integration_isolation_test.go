package controller

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// isolationIntReleaseNS is the chart's Release.Namespace: the ApplicationSet
// derives it from the directory name (apps/devplatform), so it is fixed rather
// than a value.
const isolationIntReleaseNS = "devplatform"

// isolationIntTraefikNS is where k3s' packaged Traefik runs; it is not a chart
// value because nothing in this chart can move it.
const isolationIntTraefikNS = "kube-system"

// isolationIntClusterCIDRs are this cluster's Pod and Service ranges. An egress
// ipBlock that reaches into them reaches other workspaces and other namespaces,
// which is exactly what 14.7/14.8 forbid.
var isolationIntClusterCIDRs = []string{"10.42.0.0/16", "10.43.0.0/16"}

func isolationIntChartValue(t *testing.T, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("values.yaml has no top-level %q", key)
	return ""
}

// isolationIntRender substitutes the chart placeholders the isolation manifests
// use, so the tests assert on the manifest this repository actually ships
// instead of a copy. Any placeholder this renderer does not know about fails
// the test rather than reaching the parser as literal text.
func isolationIntRender(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "templates", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	out := strings.NewReplacer(
		"{{ .Release.Namespace }}", isolationIntReleaseNS,
		"{{ .Values.workspaceNamespace }}", isolationIntChartValue(t, "workspaceNamespace"),
		"{{ .Values.subnetRouterNamespace }}", isolationIntChartValue(t, "subnetRouterNamespace"),
		// controller.clusterNodeAccess.enabled defaults to true (values.yaml);
		// this suite only exercises the shipped default, so the guard is
		// dropped rather than evaluated.
		"{{- if .Values.controller.clusterNodeAccess.enabled }}\n", "",
		"{{- end }}\n", "",
	).Replace(string(b))
	if strings.Contains(out, "{{") {
		t.Fatalf("%s contains a template directive this test cannot render; the substitutions are stale", name)
	}
	return []byte(out)
}

func isolationIntNetworkPolicy(t *testing.T) *networkingv1.NetworkPolicy {
	t.Helper()
	var np networkingv1.NetworkPolicy
	if err := utilyaml.Unmarshal(isolationIntRender(t, "workspace-networkpolicy.yaml"), &np); err != nil {
		t.Fatalf("parse workspace-networkpolicy.yaml: %v", err)
	}
	return &np
}

// isolationIntPeerReachesOwnNamespace reports whether peer admits a pod living
// in the policy's own namespace — that is, another workspace, since every
// workspace shares the one workspace namespace and is a single Pod.
func isolationIntPeerReachesOwnNamespace(t *testing.T, peer networkingv1.NetworkPolicyPeer, ownNS string) bool {
	t.Helper()
	if peer.IPBlock != nil {
		return isolationIntIPBlockCoversCluster(t, peer.IPBlock)
	}
	if peer.NamespaceSelector == nil {
		return true
	}
	if len(peer.NamespaceSelector.MatchLabels)+len(peer.NamespaceSelector.MatchExpressions) == 0 {
		return true
	}
	return peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == ownNS
}

func isolationIntIPBlockCoversCluster(t *testing.T, block *networkingv1.IPBlock) bool {
	t.Helper()
	allowed, err := netip.ParsePrefix(block.CIDR)
	if err != nil {
		t.Fatalf("parse ipBlock cidr %q: %v", block.CIDR, err)
	}
	for _, cidr := range isolationIntClusterCIDRs {
		cluster, err := netip.ParsePrefix(cidr)
		if err != nil {
			t.Fatalf("parse cluster cidr %q: %v", cidr, err)
		}
		if !allowed.Contains(cluster.Addr()) {
			continue
		}
		excepted := false
		for _, e := range block.Except {
			ex, err := netip.ParsePrefix(e)
			if err != nil {
				t.Fatalf("parse ipBlock except %q: %v", e, err)
			}
			if ex.Contains(cluster.Addr()) {
				excepted = true
				break
			}
		}
		if !excepted {
			return true
		}
	}
	return false
}

func isolationIntPortSet(ports []networkingv1.NetworkPolicyPort) map[string]bool {
	set := map[string]bool{}
	for _, p := range ports {
		proto := string(corev1.ProtocolTCP)
		if p.Protocol != nil {
			proto = string(*p.Protocol)
		}
		set[fmt.Sprintf("%s/%s", proto, p.Port)] = true
	}
	return set
}

// TestIsolationNetworkPolicyDeniesTrafficBetweenWorkspaces covers 14.7: every
// workspace shares one namespace, so a rule admitting the policy's own
// namespace admits every other workspace.
func TestIsolationNetworkPolicyDeniesTrafficBetweenWorkspaces(t *testing.T) {
	np := isolationIntNetworkPolicy(t)
	wsNS := isolationIntChartValue(t, "workspaceNamespace")

	if np.Namespace != wsNS {
		t.Fatalf("policy namespace = %q, want the workspace namespace %q", np.Namespace, wsNS)
	}
	if len(np.Spec.PodSelector.MatchLabels)+len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("podSelector = %+v, want empty so the policy covers every workspace Pod", np.Spec.PodSelector)
	}
	for _, want := range []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress} {
		found := false
		for _, got := range np.Spec.PolicyTypes {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("policyTypes = %v, want %s included so that direction defaults to deny", np.Spec.PolicyTypes, want)
		}
	}

	for i, rule := range np.Spec.Ingress {
		if len(rule.From) == 0 {
			t.Errorf("ingress rule %d has no peers, which admits every source", i)
		}
		for _, peer := range rule.From {
			if isolationIntPeerReachesOwnNamespace(t, peer, np.Namespace) {
				t.Errorf("ingress rule %d admits peer %+v inside the workspace namespace; workspace-to-workspace traffic must be denied", i, peer)
			}
		}
	}
	for i, rule := range np.Spec.Egress {
		if len(rule.To) == 0 {
			t.Errorf("egress rule %d has no peers, which admits every destination", i)
		}
		for _, peer := range rule.To {
			if isolationIntPeerReachesOwnNamespace(t, peer, np.Namespace) {
				t.Errorf("egress rule %d admits peer %+v inside the workspace namespace; workspace-to-workspace traffic must be denied", i, peer)
			}
		}
	}
}

// TestIsolationNetworkPolicyLimitsCrossNamespaceReach covers 14.8: the only
// namespaces a workspace may reach are the ones the design names, on the ports
// those services listen on, and nothing may route into the cluster's own
// address ranges.
func TestIsolationNetworkPolicyLimitsCrossNamespaceReach(t *testing.T) {
	np := isolationIntNetworkPolicy(t)

	wantIngress := map[string]map[string]bool{
		isolationIntReleaseNS:                              {"TCP/8787": true},
		isolationIntChartValue(t, "subnetRouterNamespace"): {"TCP/22": true},
		isolationIntTraefikNS:                              isolationIntPreviewReportPorts(),
	}
	wantEgress := map[string]map[string]bool{
		"devplatform-db": {"TCP/5432": true},
		"garage":         {"TCP/3900": true},
		"kube-system":    {"UDP/53": true, "TCP/53": true, "TCP/443": true},
	}

	gotIngress := map[string]map[string]bool{}
	for _, rule := range np.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.NamespaceSelector == nil {
				continue
			}
			gotIngress[peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]] = isolationIntPortSet(rule.Ports)
		}
	}
	if diff := fmt.Sprint(gotIngress); diff != fmt.Sprint(wantIngress) {
		t.Errorf("ingress from other namespaces = %v, want %v", gotIngress, wantIngress)
	}

	gotEgress := map[string]map[string]bool{}
	externalRules := 0
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			switch {
			case peer.NamespaceSelector != nil:
				gotEgress[peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]] = isolationIntPortSet(rule.Ports)
			case peer.IPBlock != nil:
				externalRules++
				if isolationIntIPBlockCoversCluster(t, peer.IPBlock) {
					t.Errorf("egress ipBlock %+v reaches the cluster's own address ranges; other namespaces must stay unreachable", peer.IPBlock)
				}
				if got, want := isolationIntPortSet(rule.Ports), map[string]bool{"TCP/443": true, "TCP/53": true, "UDP/53": true}; fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("external egress ports = %v, want %v", got, want)
				}
			}
		}
	}
	if fmt.Sprint(gotEgress) != fmt.Sprint(wantEgress) {
		t.Errorf("egress to other namespaces = %v, want %v", gotEgress, wantEgress)
	}
	if externalRules != 1 {
		t.Errorf("external egress rules = %d, want exactly 1", externalRules)
	}
}

// isolationIntPreviewReportPorts is derived from ingress_reconciler.go's own
// constants so the policy cannot drift away from the ports the generated
// IngressRoutes actually forward to.
func isolationIntPreviewReportPorts() map[string]bool {
	return map[string]bool{
		fmt.Sprintf("TCP/%d", previewServicePort): true,
		fmt.Sprintf("TCP/%d", reportServicePort):  true,
	}
}

// TestIsolationNetworkPolicyAdmitsTraefikToPreviewAndReport covers 11.1/11.5:
// the per-workspace IngressRoutes are inert unless Traefik itself may reach the
// preview and report ports, and that exception must not widen to any other
// source — including another workspace in the same namespace.
func TestIsolationNetworkPolicyAdmitsTraefikToPreviewAndReport(t *testing.T) {
	np := isolationIntNetworkPolicy(t)
	wantPorts := isolationIntPreviewReportPorts()

	matched := 0
	for i, rule := range np.Spec.Ingress {
		ports := isolationIntPortSet(rule.Ports)
		serving := false
		for port := range wantPorts {
			if ports[port] {
				serving = true
			}
		}
		if !serving {
			continue
		}
		matched++

		if fmt.Sprint(ports) != fmt.Sprint(wantPorts) {
			t.Errorf("ingress rule %d ports = %v, want exactly %v", i, ports, wantPorts)
		}
		for _, peer := range rule.From {
			if isolationIntPeerReachesOwnNamespace(t, peer, np.Namespace) {
				t.Errorf("ingress rule %d admits peer %+v inside the workspace namespace on the preview/report ports", i, peer)
				continue
			}
			if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != isolationIntTraefikNS {
				t.Errorf("ingress rule %d admits peer %+v; only %s may reach the preview/report ports", i, peer, isolationIntTraefikNS)
				continue
			}
			if peer.PodSelector == nil || peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "traefik" {
				t.Errorf("ingress rule %d peer %+v is not narrowed to the Traefik Pods", i, peer)
			}
		}
	}
	if matched != 1 {
		t.Errorf("ingress rules serving the preview/report ports = %d, want exactly 1", matched)
	}
}

func isolationIntEnsureNamespace(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	err := testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// isolationIntApplyRBAC installs the shipped ServiceAccount/Role/RoleBinding
// exactly as rendered, so the denials below are produced by the real manifest
// rather than by a hand-written fixture.
func isolationIntApplyRBAC(t *testing.T, ctx context.Context) {
	t.Helper()
	docs := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(isolationIntRender(t, "controller-rbac.yaml"))), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := docs.Decode(obj); err != nil {
			break
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := testClient.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("apply %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
}

func isolationIntOperatorClient(t *testing.T) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(testCfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + isolationIntReleaseNS + ":devplatform-workspace-operator",
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("build impersonating client: %v", err)
	}
	return c
}

// TestIsolationWorkspaceOperatorIsDeniedOutsideItsScope covers 14.8/14.9: the
// API server itself, not the controller's own code, must refuse anything the
// workspace-operator ServiceAccount was not granted.
func TestIsolationWorkspaceOperatorIsDeniedOutsideItsScope(t *testing.T) {
	ctx := context.Background()
	wsNS := isolationIntChartValue(t, "workspaceNamespace")
	isolationIntEnsureNamespace(t, ctx, isolationIntReleaseNS)
	isolationIntEnsureNamespace(t, ctx, wsNS)
	isolationIntApplyRBAC(t, ctx)
	otherNS := newNamespace(t)

	operator := isolationIntOperatorClient(t)

	allowed := map[string]func() error{
		"list Workspaces in the workspace namespace": func() error {
			return operator.List(ctx, &devplatformv1alpha1.WorkspaceList{}, client.InNamespace(wsNS))
		},
		"list Secrets in the workspace namespace": func() error {
			return operator.List(ctx, &corev1.SecretList{}, client.InNamespace(wsNS))
		},
		"list Nodes for image pre-staging": func() error {
			return operator.List(ctx, &corev1.NodeList{})
		},
	}
	for name, attempt := range allowed {
		if err := attempt(); err != nil {
			t.Errorf("%s: %v, want it to be permitted", name, err)
		}
	}

	denied := map[string]func() error{
		"read Secrets in another namespace": func() error {
			return operator.List(ctx, &corev1.SecretList{}, client.InNamespace(otherNS))
		},
		"reach Services in another namespace": func() error {
			return operator.List(ctx, &corev1.ServiceList{}, client.InNamespace(otherNS))
		},
		"create a Workspace in another namespace": func() error {
			return operator.Create(ctx, &devplatformv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: "escape", Namespace: otherNS},
				Spec:       devplatformv1alpha1.WorkspaceSpec{Repository: "https://example.com/r.git", Branch: "feature/x", TemplateRef: "default"},
			})
		},
		"create a Pod directly in the workspace namespace": func() error {
			return operator.Create(ctx, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "escape", Namespace: wsNS},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
			})
		},
		"enumerate the cluster's namespaces": func() error {
			return operator.List(ctx, &corev1.NamespaceList{})
		},
		"grant itself cluster-wide privileges": func() error {
			return operator.Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "escape"},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
			})
		},
		"delete a Node": func() error {
			return operator.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k3s-agent-z440"}})
		},
	}
	for name, attempt := range denied {
		err := attempt()
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s: err = %v, want a Forbidden refusal from the API server", name, err)
		}
	}
}

// TestIsolationGitCredentialRefusesDefaultBranchPush covers 14.13: a workspace
// whose working branch is the repository's default branch would otherwise be
// handed a credential permitted to push there.
func TestIsolationGitCredentialRefusesDefaultBranchPush(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	repo := "https://gitea.fickledev.com/tom1022/demo.git"

	for i, tc := range []struct {
		branch     string
		baseBranch string
	}{
		{branch: "main"},
		{branch: "master"},
		{branch: "Main"},
		{branch: "release/1.0", baseBranch: "release/1.0"},
	} {
		name := fmt.Sprintf("ws-defbranch-%d", i)
		ws := createWorkspace(t, ctx, ns, name, repo, tc.branch, "default")
		if tc.baseBranch != "" {
			ws.Spec.BaseBranch = &tc.baseBranch
		}

		fake := &fakeGitCredentialIssuer{}
		r := newTestReconciler()
		r.GitCredentialIssuer = fake

		if err := r.reconcileGitCredential(ctx, ws, name); err == nil {
			t.Errorf("reconcileGitCredential(branch=%q, base=%q) = nil, want a refusal", tc.branch, tc.baseBranch)
		}
		if len(fake.requests) != 0 {
			t.Errorf("branch %q reached the issuer; no credential may be requested for a default branch", tc.branch)
		}
		var secret corev1.Secret
		if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &secret); !apierrors.IsNotFound(err) {
			t.Errorf("branch %q left a credential Secret behind (err=%v)", tc.branch, err)
		}
	}

	base := "main"
	ws := createWorkspace(t, ctx, ns, "ws-defbranch-ok", repo, "feature/scoped", "default")
	ws.Spec.BaseBranch = &base
	fake := &fakeGitCredentialIssuer{}
	r := newTestReconciler()
	r.GitCredentialIssuer = fake
	if err := r.reconcileGitCredential(ctx, ws, "ws-defbranch-ok"); err != nil {
		t.Fatalf("a working branch forked from the default branch must still be issued: %v", err)
	}
	if len(fake.requests) != 1 || fake.requests[0].AllowedBranch != "feature/scoped" {
		t.Errorf("issued scope = %+v, want push limited to the working branch", fake.requests)
	}
}
