package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// testEnv boots a real (kubelet-less) kube-apiserver + etcd from the CRD YAML
// under apps/devplatform/templates/, which stays the single source of truth
// for the schema (no generated/duplicated CRD copy in this module).
var testEnv *envtest.Environment
var testClient client.Client

func TestMain(m *testing.M) {
	os.Exit(runWithEnvtest(m))
}

// runWithEnvtest exists so testEnv.Stop() runs on every exit path, which a
// bare os.Exit(m.Run()) in TestMain would skip.
func runWithEnvtest(m *testing.M) int {
	if err := devplatformv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}

	testEnv = &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{
				filepath.Join("..", "..", "..", "templates", "crd-workspace.yaml"),
				filepath.Join("..", "..", "..", "templates", "crd-workspacetemplate.yaml"),
				filepath.Join("..", "..", "..", "templates", "crd-taskrequest.yaml"),
				// The CNPG operator itself is not running under envtest (no
				// controller reconciles Database/DatabaseRole into real Postgres
				// state), but installing its CRDs from the manifest this cluster
				// actually deploys lets tests create and assert on the generated
				// spec content with real schema validation.
				filepath.Join("..", "..", "..", "..", "cnpg-operator", "cnpg-operator.yaml"),
				// Traefik ships as part of k3s itself (no GitOps-managed manifest
				// to point at), so its IngressRoute CRD is trimmed to the fields
				// this controller sets and vendored as a test fixture instead.
				filepath.Join("..", "..", "hack", "traefik-ingressroute-crd.yaml"),
			},
			ErrorIfPathMissing: true,
		},
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic(err)
	}
	defer func() { _ = testEnv.Stop() }()

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic(err)
	}
	testClient = c

	return m.Run()
}

// newNamespace creates a fresh, server-named namespace so tests can run
// without their Workspace/WorkspaceTemplate objects interfering with each
// other.
func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "ws-test-"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := testClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}
