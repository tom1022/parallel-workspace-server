package gateway

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func testWorkspace(name, branch, workspaceID, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": workspaceGVR.Group + "/" + workspaceGVR.Version,
		"kind":       "Workspace",
		"metadata":   map[string]any{"name": name, "namespace": "devplatform-workspaces"},
		"spec": map[string]any{
			"repository":  "https://gitea.example.com/tochi/portfolio.git",
			"branch":      branch,
			"templateRef": "default",
		},
		"status": map[string]any{
			"phase":       phase,
			"workspaceId": workspaceID,
			"sessionId":   workspaceID + "-session",
			"urls":        map[string]any{"session": "https://" + workspaceID + ".fickledev.com"},
		},
	}}
}

func newTestStore(t *testing.T, objs ...runtime.Object) *Store {
	t.Helper()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schemaGVR]string{
		workspaceGVR: "WorkspaceList",
		podGVR:       "PodList",
	}, objs...)
	return &Store{Dynamic: client, Namespace: "devplatform-workspaces", DefaultTemplateRef: "default"}
}

func TestResolveHostFindsWorkspaceByLeadingLabel(t *testing.T) {
	store := newTestStore(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))

	for _, host := range []string{"feature-login.fickledev.com", "feature-login.fickledev.com:443", "FEATURE-LOGIN.fickledev.com"} {
		ws, err := store.ResolveHost(context.Background(), host)
		if err != nil {
			t.Fatalf("ResolveHost(%q): %v", host, err)
		}
		if ws.WorkspaceId != "feature-login" {
			t.Fatalf("ResolveHost(%q) = %q", host, ws.WorkspaceId)
		}
	}
}

func TestResolveHostRejectsUnknownAndEmptyHosts(t *testing.T) {
	store := newTestStore(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	for _, host := range []string{"", "nope.fickledev.com", "fickledev.com."} {
		if _, err := store.ResolveHost(context.Background(), host); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveHost(%q) error = %v, want ErrNotFound", host, err)
		}
	}
}

func TestListReportsEveryWorkspace(t *testing.T) {
	store := newTestStore(t,
		testWorkspace("feature-login", "feature/login", "feature-login", "Ready"),
		testWorkspace("fix-crash", "fix/crash", "fix-crash", "Provisioning"),
	)
	got, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d workspaces, want 2", len(got))
	}
	if got[0].Branch == "" || got[0].Phase == "" || got[0].Repository == "" {
		t.Fatalf("summary is missing fields: %+v", got[0])
	}
}

func TestCreateDerivesNameFromBranchAndRejectsDuplicates(t *testing.T) {
	store := newTestStore(t)
	req := CreateWorkspaceRequest{Repository: "https://gitea.example.com/tochi/portfolio.git", Branch: "feature/Login"}

	created, err := store.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Name != "feature-login" {
		t.Fatalf("Name = %q, want feature-login", created.Name)
	}
	if created.TemplateRef != "default" {
		t.Fatalf("TemplateRef = %q, want the configured default", created.TemplateRef)
	}

	if _, err := store.Create(context.Background(), req); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Create error = %v, want ErrAlreadyExists", err)
	}
}

func TestCreateRequiresRepositoryAndBranch(t *testing.T) {
	store := newTestStore(t)
	for name, req := range map[string]CreateWorkspaceRequest{
		"no repository": {Branch: "feature/login"},
		"no branch":     {Repository: "https://gitea.example.com/tochi/portfolio.git"},
		"unnameable":    {Repository: "https://gitea.example.com/tochi/portfolio.git", Branch: "///"},
	} {
		if _, err := store.Create(context.Background(), req); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestDeleteRemovesWorkspaceAndReportsMissingOnes(t *testing.T) {
	store := newTestStore(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))

	if err := store.Delete(context.Background(), "feature-login"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(context.Background(), "feature-login"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete error = %v, want ErrNotFound", err)
	}
}

func TestFindFallsBackToStatusWorkspaceId(t *testing.T) {
	// The object name and the derived workspace identifier diverge whenever the
	// controller had to break a name collision, so callers that only know the
	// identifier must still resolve.
	store := newTestStore(t, testWorkspace("login-2", "feature/login", "feature-login-a1b2c3d4", "Ready"))

	byName, err := store.Find(context.Background(), "login-2")
	if err != nil {
		t.Fatalf("Find by name: %v", err)
	}
	byID, err := store.Find(context.Background(), "feature-login-a1b2c3d4")
	if err != nil {
		t.Fatalf("Find by workspaceId: %v", err)
	}
	if byName.Name != byID.Name {
		t.Fatalf("Find disagreed: %q vs %q", byName.Name, byID.Name)
	}
	if _, err := store.Find(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Find(absent) error = %v, want ErrNotFound", err)
	}
}

func TestSessionEndpointUsesRunningPodAddress(t *testing.T) {
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      "feature-login-0",
			"namespace": "devplatform-workspaces",
			"labels":    map[string]any{labelWorkspaceName: "feature-login"},
		},
		"status": map[string]any{"phase": "Running", "podIP": "10.42.0.9"},
	}}
	store := newTestStore(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"), pod)

	ws, err := store.Find(context.Background(), "feature-login")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := store.SessionEndpoint(context.Background(), ws)
	if err != nil {
		t.Fatalf("SessionEndpoint: %v", err)
	}
	if endpoint != "http://10.42.0.9:8787" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestSessionEndpointFailsWhileNoPodIsRunning(t *testing.T) {
	store := newTestStore(t, testWorkspace("feature-login", "feature/login", "feature-login", "Provisioning"))
	ws, err := store.Find(context.Background(), "feature-login")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionEndpoint(context.Background(), ws); err == nil {
		t.Fatal("SessionEndpoint succeeded with no pod, want error")
	}
}
