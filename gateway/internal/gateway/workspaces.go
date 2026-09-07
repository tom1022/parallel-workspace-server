package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// schemaGVR is aliased so the test helper and this file agree on the map key
// type of the fake dynamic client without importing the schema package twice.
type schemaGVR = schema.GroupVersionResource

var (
	workspaceGVR = schema.GroupVersionResource{Group: "devplatform.fickledev.com", Version: "v1alpha1", Resource: "workspaces"}
	podGVR       = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
)

// labelWorkspaceName is the label the Workspace Controller puts on everything it
// owns (controller/internal/controller/resources.go), and is how the gateway
// finds a workspace's Pod without going through the control plane (4.9).
const labelWorkspaceName = "devplatform.fickledev.com/workspace"

// annotationBrowserConnections carries the number of browser connections this
// gateway holds against a workspace. The Workspace Controller reads it as an
// idle-detection input (4.8). It is an annotation rather than a status write
// because the controller is the only writer of Workspace status.
const annotationBrowserConnections = "devplatform.fickledev.com/browser-connections"

// defaultSupervisorPort is the Session Supervisor's listen port inside the
// workspace Pod. It must match the workspace base image and the controller's
// container port declaration.
const defaultSupervisorPort = 8787

var (
	ErrNotFound      = errors.New("gateway: workspace not found")
	ErrAlreadyExists = errors.New("gateway: workspace already exists")
	ErrInvalid       = errors.New("gateway: invalid workspace request")
)

// WorkspaceURLs mirrors Workspace.status.urls.
type WorkspaceURLs struct {
	Preview string `json:"preview,omitempty"`
	Session string `json:"session,omitempty"`
	Report  string `json:"report,omitempty"`
}

// WorkspaceSummary is what GET/POST /api/workspaces answer with.
type WorkspaceSummary struct {
	Name        string        `json:"name"`
	WorkspaceId string        `json:"workspaceId,omitempty"`
	Repository  string        `json:"repository"`
	Branch      string        `json:"branch"`
	TemplateRef string        `json:"templateRef"`
	Phase       string        `json:"phase,omitempty"`
	SessionId   string        `json:"sessionId,omitempty"`
	Urls        WorkspaceURLs `json:"urls"`
}

// CreateWorkspaceRequest is the body of POST /api/workspaces.
type CreateWorkspaceRequest struct {
	Repository  string `json:"repository"`
	Branch      string `json:"branch"`
	BaseBranch  string `json:"baseBranch,omitempty"`
	TemplateRef string `json:"templateRef,omitempty"`
}

// Store is the gateway's only state access. Everything it answers comes from the
// Kubernetes API, so an already-provisioned workspace stays reachable and
// manageable while the Workspace Controller and Hermes Agent are down (4.9 /
// 19.8).
type Store struct {
	Dynamic            dynamic.Interface
	Namespace          string
	DefaultTemplateRef string
	// SupervisorPort overrides defaultSupervisorPort.
	SupervisorPort int
}

func (s *Store) workspaces() dynamic.ResourceInterface {
	return s.Dynamic.Resource(workspaceGVR).Namespace(s.Namespace)
}

// List returns every workspace in the workspace namespace.
func (s *Store) List(ctx context.Context) ([]WorkspaceSummary, error) {
	list, err := s.workspaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	summaries := make([]WorkspaceSummary, 0, len(list.Items))
	for i := range list.Items {
		summaries = append(summaries, summarize(&list.Items[i]))
	}
	return summaries, nil
}

// Find resolves id as either the Workspace object's name or its derived
// status.workspaceId. The two diverge whenever the controller had to break a
// name collision, and callers hold either one.
func (s *Store) Find(ctx context.Context, id string) (WorkspaceSummary, error) {
	if id == "" {
		return WorkspaceSummary{}, ErrNotFound
	}
	obj, err := s.workspaces().Get(ctx, id, metav1.GetOptions{})
	if err == nil {
		return summarize(obj), nil
	}
	if !apierrors.IsNotFound(err) {
		return WorkspaceSummary{}, err
	}

	all, err := s.List(ctx)
	if err != nil {
		return WorkspaceSummary{}, err
	}
	for _, ws := range all {
		if ws.WorkspaceId == id {
			return ws, nil
		}
	}
	return WorkspaceSummary{}, ErrNotFound
}

// ResolveHost maps an inbound Host header to the workspace it addresses. Only
// the leading DNS label carries the identity: the wildcard certificate covers a
// single label under fickledev.com, so nothing deeper can appear here.
func (s *Store) ResolveHost(ctx context.Context, host string) (WorkspaceSummary, error) {
	label, _, _ := strings.Cut(strings.ToLower(hostWithoutPort(host)), ".")
	if label == "" {
		return WorkspaceSummary{}, ErrNotFound
	}
	return s.Find(ctx, label)
}

// Create declares a new workspace. Beyond the two fields needed to name the
// object, the body is left to the Workspace CRD's own validation rather than
// re-stated here.
func (s *Store) Create(ctx context.Context, req CreateWorkspaceRequest) (WorkspaceSummary, error) {
	if req.Repository == "" || req.Branch == "" {
		return WorkspaceSummary{}, fmt.Errorf("%w: repository and branch are required", ErrInvalid)
	}
	name := sanitizeDNSLabel(req.Branch)
	if name == "" {
		return WorkspaceSummary{}, fmt.Errorf("%w: branch %q yields no usable resource name", ErrInvalid, req.Branch)
	}
	templateRef := req.TemplateRef
	if templateRef == "" {
		templateRef = s.DefaultTemplateRef
	}

	spec := map[string]any{
		"repository":  req.Repository,
		"branch":      req.Branch,
		"templateRef": templateRef,
	}
	if req.BaseBranch != "" {
		spec["baseBranch"] = req.BaseBranch
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": workspaceGVR.Group + "/" + workspaceGVR.Version,
		"kind":       "Workspace",
		"metadata":   map[string]any{"name": name, "namespace": s.Namespace},
		"spec":       spec,
	}}

	created, err := s.workspaces().Create(ctx, obj, metav1.CreateOptions{})
	// The object name is derived from the branch, so the API server's
	// uniqueness check is also the "one workspace per branch" check (409).
	if apierrors.IsAlreadyExists(err) {
		return WorkspaceSummary{}, ErrAlreadyExists
	}
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return WorkspaceSummary{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	if err != nil {
		return WorkspaceSummary{}, err
	}
	return summarize(created), nil
}

// Delete removes a workspace. Its dependent resources follow by owner-reference
// garbage collection, so no control plane component has to be reachable.
func (s *Store) Delete(ctx context.Context, id string) error {
	ws, err := s.Find(ctx, id)
	if err != nil {
		return err
	}
	err = s.workspaces().Delete(ctx, ws.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

// SessionEndpoint is the base URL of the workspace's Session Supervisor. The
// Pod address is read straight from the Kubernetes API rather than through a
// Service: the controller creates no Service for a workspace, and adding a DNS
// hop would put another moving part on the one path that must survive a control
// plane outage.
func (s *Store) SessionEndpoint(ctx context.Context, ws WorkspaceSummary) (string, error) {
	pods, err := s.Dynamic.Resource(podGVR).Namespace(s.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelWorkspaceName + "=" + ws.Name,
	})
	if err != nil {
		return "", err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
		ip, _, _ := unstructured.NestedString(pod.Object, "status", "podIP")
		if phase == "Running" && ip != "" {
			port := s.SupervisorPort
			if port == 0 {
				port = defaultSupervisorPort
			}
			return fmt.Sprintf("http://%s:%d", ip, port), nil
		}
	}
	return "", fmt.Errorf("gateway: workspace %q has no running session pod", ws.Name)
}

// SetBrowserConnections records how many browser connections are open against
// a workspace right now. A count of zero removes the annotation instead of
// writing "0", so a workspace nobody is on looks the same as one that was
// never connected to.
func (s *Store) SetBrowserConnections(ctx context.Context, name string, count int) error {
	var value any
	if count > 0 {
		value = strconv.Itoa(count)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{annotationBrowserConnections: value},
		},
	})
	if err != nil {
		return err
	}
	_, err = s.workspaces().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

func summarize(obj *unstructured.Unstructured) WorkspaceSummary {
	str := func(fields ...string) string {
		v, _, _ := unstructured.NestedString(obj.Object, fields...)
		return v
	}
	return WorkspaceSummary{
		Name:        obj.GetName(),
		WorkspaceId: str("status", "workspaceId"),
		Repository:  str("spec", "repository"),
		Branch:      str("spec", "branch"),
		TemplateRef: str("spec", "templateRef"),
		Phase:       str("status", "phase"),
		SessionId:   str("status", "sessionId"),
		Urls: WorkspaceURLs{
			Preview: str("status", "urls", "preview"),
			Session: str("status", "urls", "session"),
			Report:  str("status", "urls", "report"),
		},
	}
}

func hostWithoutPort(host string) string {
	if h, _, found := strings.Cut(host, ":"); found {
		return h
	}
	return host
}

var nonDNSLabelChars = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeDNSLabel mirrors DeriveResourceName's normalization in the controller
// module. It is duplicated rather than shared because the gateway must not
// depend on the control plane's code, and it only needs the collision-free
// case: a name that does collide is exactly the 409 this endpoint owes its
// caller.
func sanitizeDNSLabel(s string) string {
	lower := strings.ToLower(s)
	collapsed := regexp.MustCompile(`-+`).ReplaceAllString(nonDNSLabelChars.ReplaceAllString(lower, "-"), "-")
	trimmed := strings.Trim(collapsed, "-")
	if len(trimmed) > 63 {
		trimmed = strings.TrimRight(trimmed[:63], "-")
	}
	return trimmed
}
