package controller

import (
	"strings"
	"testing"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

func supervisorTestWorkspace() (*devplatformv1alpha1.Workspace, *devplatformv1alpha1.WorkspaceTemplate) {
	ws := &devplatformv1alpha1.Workspace{
		Spec: devplatformv1alpha1.WorkspaceSpec{
			Repository: "https://gitea.fickledev.com/tom1022/demo.git",
			Branch:     "feature/supervisor",
		},
	}
	ws.Name = "feature-supervisor"
	tmpl := &devplatformv1alpha1.WorkspaceTemplate{
		Spec: devplatformv1alpha1.WorkspaceTemplateSpec{
			Image:    "busybox:1.36",
			NodeName: "test-node",
			Resources: devplatformv1alpha1.WorkspaceResources{
				Requests: devplatformv1alpha1.ResourceList{CPU: "100m", Memory: "128Mi"},
				Limits:   devplatformv1alpha1.ResourceList{CPU: "500m", Memory: "256Mi"},
			},
			Auth: devplatformv1alpha1.WorkspaceAuthRef{SecretRef: "claude-auth"},
		},
	}
	return ws, tmpl
}

// TestBuildStatefulSet_WorkspaceRunsSessionSupervisor covers 2.1: the
// workspace container's entrypoint is the Session Supervisor, which is what
// keeps a Claude Code session resident regardless of client attachment.
func TestBuildStatefulSet_WorkspaceRunsSessionSupervisor(t *testing.T) {
	ws, tmpl := supervisorTestWorkspace()

	sts, err := buildStatefulSet(ws, tmpl, "feature-supervisor")
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected exactly 1 container, got %d", len(containers))
	}
	c := containers[0]
	if len(c.Command) != 1 || c.Command[0] != supervisorBinaryPath {
		t.Errorf("command = %v, want the session supervisor at %q", c.Command, supervisorBinaryPath)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["WORKSPACE_MOUNT"] != workspaceMountPath {
		t.Errorf("WORKSPACE_MOUNT = %q, want %q", env["WORKSPACE_MOUNT"], workspaceMountPath)
	}
	if env["WORKSPACE_NAME"] != ws.Name {
		t.Errorf("WORKSPACE_NAME = %q, want %q", env["WORKSPACE_NAME"], ws.Name)
	}
	// 5.2: the credential comes from the read-only Secret mount.
	if env["CLAUDE_AUTH_FILE"] != authMountPath+"/credentials.json" {
		t.Errorf("CLAUDE_AUTH_FILE = %q", env["CLAUDE_AUTH_FILE"])
	}

	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != supervisorPort {
		t.Errorf("ports = %+v, want the supervisor control API on %d", c.Ports, supervisorPort)
	}
}

// 7.13/7.14: the checkout path reaches Claude Code's system prompt, so it has
// to be the same string for every workspace or the prompt cache never hits.
func TestBuildStatefulSet_PathsCarryNoWorkspaceIdentity(t *testing.T) {
	ws, tmpl := supervisorTestWorkspace()
	other, _ := supervisorTestWorkspace()
	other.Name = "hotfix-login"
	other.Spec.Branch = "hotfix/login"

	a, err := buildStatefulSet(ws, tmpl, ws.Name)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildStatefulSet(other, tmpl, other.Name)
	if err != nil {
		t.Fatal(err)
	}

	envA := map[string]string{}
	for _, e := range a.Spec.Template.Spec.InitContainers[0].Env {
		envA[e.Name] = e.Value
	}
	envB := map[string]string{}
	for _, e := range b.Spec.Template.Spec.InitContainers[0].Env {
		envB[e.Name] = e.Value
	}
	if envA["WORKSPACE_DIR"] != envB["WORKSPACE_DIR"] {
		t.Errorf("checkout path differs per workspace: %q vs %q", envA["WORKSPACE_DIR"], envB["WORKSPACE_DIR"])
	}
	for _, ident := range []string{"feature", "supervisor", "hotfix"} {
		if strings.Contains(envA["WORKSPACE_DIR"], ident) {
			t.Errorf("checkout path %q embeds workspace identity", envA["WORKSPACE_DIR"])
		}
	}
}

// The defect this guards: with the checkout at the volume root, the config
// area, the session log and the package cache are untracked files inside the
// developer's git tree.
func TestBuildStatefulSet_PlatformStateLivesOutsideTheCheckout(t *testing.T) {
	ws, tmpl := supervisorTestWorkspace()
	sts, err := buildStatefulSet(ws, tmpl, ws.Name)
	if err != nil {
		t.Fatal(err)
	}

	if workingDirPath == workspaceMountPath {
		t.Fatal("the checkout must not be the volume root")
	}
	for name, dir := range map[string]string{"config": claudeConfigPath, "cache": cacheDirPath} {
		if strings.HasPrefix(dir, workingDirPath+"/") || dir == workingDirPath {
			t.Errorf("%s dir %q sits inside the checkout %q", name, dir, workingDirPath)
		}
		if !strings.HasPrefix(dir, workspaceMountPath+"/") {
			t.Errorf("%s dir %q must stay on the workspace volume (5.3, 15.12)", name, dir)
		}
	}

	init := sts.Spec.Template.Spec.InitContainers[0]
	env := map[string]string{}
	for _, e := range init.Env {
		env[e.Name] = e.Value
	}
	if env["WORKSPACE_DIR"] != workingDirPath {
		t.Errorf("init WORKSPACE_DIR = %q, want the checkout %q", env["WORKSPACE_DIR"], workingDirPath)
	}
	if env["CLAUDE_CONFIG_DIR"] != claudeConfigPath {
		t.Errorf("init CLAUDE_CONFIG_DIR = %q, want %q", env["CLAUDE_CONFIG_DIR"], claudeConfigPath)
	}
	if !strings.Contains(workspaceInitScript, "$WORKSPACE_CACHE_DIR") {
		t.Error("the init script must create the package cache dir on the volume (15.12)")
	}
}

// 7.3: the default model is operator-configurable per template.
func TestBuildStatefulSet_DefaultModelComesFromTheTemplate(t *testing.T) {
	ws, tmpl := supervisorTestWorkspace()
	tmpl.Spec.Model = "claude-sonnet-5"

	sts, err := buildStatefulSet(ws, tmpl, ws.Name)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["ANTHROPIC_MODEL"] != "claude-sonnet-5" {
		t.Errorf("ANTHROPIC_MODEL = %q, want the template's model", env["ANTHROPIC_MODEL"])
	}
}

func TestBuildStatefulSet_UnsetModelLeavesClaudeCodeDefault(t *testing.T) {
	ws, tmpl := supervisorTestWorkspace()

	sts, err := buildStatefulSet(ws, tmpl, ws.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "ANTHROPIC_MODEL" {
			t.Errorf("ANTHROPIC_MODEL = %q, want it absent so Claude Code picks its own default", e.Value)
		}
	}
}
