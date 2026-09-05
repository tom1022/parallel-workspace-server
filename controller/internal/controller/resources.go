package controller

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// workspaceMountPath is where the workspace PVC is mounted. The layout
	// below it is fixed and identical in every workspace: the checkout path
	// reaches Claude Code's system prompt, and the prompt cache is only reused
	// across workspaces when that prefix matches byte for byte (7.13, 7.14).
	workspaceMountPath = "/workspace"

	// workingDirPath is the branch's git checkout. It is a subdirectory rather
	// than the mount root because the config area, the session output log and
	// the package cache all have to live on the same volume (5.3, 15.12) — at
	// the root they would sit inside the git tree as untracked files, where a
	// `git add -A` in the session would commit the credential.
	workingDirPath = workspaceMountPath + "/repo"

	// claudeConfigPath keeps Claude Code's config area on the per-workspace PVC
	// so it stays independent per workspace and survives restarts (5.3),
	// distinct from the read-only mounted long-lived credential in
	// authMountPath. cacheDirPath is the package cache the supervisor points
	// every package manager at, so a suspend/resume does not re-download
	// (15.12). Both mirror session.Paths in the supervisor module.
	claudeConfigPath = workspaceMountPath + "/.claude-config"
	cacheDirPath     = workspaceMountPath + "/.cache"

	authMountPath        = "/run/devplatform/claude-auth"
	authSecretVolumeName = "claude-auth"
	workspaceVolumeName  = "workspace"

	labelWorkspaceName = "devplatform.fickledev.com/workspace"

	// supervisorBinaryPath and supervisorPort must match the workspace base
	// image (apps/devplatform/image/workspace/Dockerfile) and the Session
	// Supervisor's own default listen address.
	supervisorBinaryPath = "/usr/local/bin/supervisor"
	supervisorPort       = 8787

	localPathStorageClass = "local-path"

	defaultWorkspaceStorageSize = "20Gi"
)

// workspaceLabels are applied to every resource the controller creates for ws,
// and are how a Workspace's owned substrate is found back (Requirement 1.10).
func workspaceLabels(ws *devplatformv1alpha1.Workspace) map[string]string {
	return map[string]string{
		labelWorkspaceName: ws.Name,
	}
}

func buildPVC(ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate, resourceName string) (*corev1.PersistentVolumeClaim, error) {
	size := tmpl.Spec.Storage.Size
	if size == "" {
		size = defaultWorkspaceStorageSize
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("devplatform: invalid storage size %q on template %q: %w", size, tmpl.Name, err)
	}

	storageClass := localPathStorageClass
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: ws.Namespace,
			Labels:    workspaceLabels(ws),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: qty,
				},
			},
		},
	}, nil
}

func parseResourceList(rl devplatformv1alpha1.ResourceList) (corev1.ResourceList, error) {
	cpu, err := resource.ParseQuantity(rl.CPU)
	if err != nil {
		return nil, fmt.Errorf("devplatform: invalid cpu quantity %q: %w", rl.CPU, err)
	}
	mem, err := resource.ParseQuantity(rl.Memory)
	if err != nil {
		return nil, fmt.Errorf("devplatform: invalid memory quantity %q: %w", rl.Memory, err)
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    cpu,
		corev1.ResourceMemory: mem,
	}, nil
}

// workspaceInitScript expands the target branch into the (initially empty)
// working directory and clears any credential left over from a prior
// container instance before Claude Code starts (5.4). Idempotent: re-running
// on an already-initialized PVC (e.g. Pod restart) is a no-op for the clone
// step. Git remote credentials themselves are out of scope here (task 2.4).
//
// It also runs on every container (re)start, including a crash-loop restart
// of the same Pod and a Suspended->Ready resume's fresh Pod — the only points
// a stale git lock from a killed clone/checkout can be cleared before the
// next git operation in this same script would otherwise fail against it
// (16.5).
const workspaceInitScript = `set -eu
mkdir -p "$WORKSPACE_DIR" "$CLAUDE_CONFIG_DIR" "$WORKSPACE_CACHE_DIR"
rm -f "$WORKSPACE_DIR/.git/index.lock" "$WORKSPACE_DIR/.git/HEAD.lock" "$WORKSPACE_DIR/.git/shallow.lock"
if [ ! -d "$WORKSPACE_DIR/.git" ]; then
  if git ls-remote --exit-code --heads "$WORKSPACE_REPOSITORY" "$WORKSPACE_BRANCH" >/dev/null 2>&1; then
    git clone --branch "$WORKSPACE_BRANCH" --single-branch "$WORKSPACE_REPOSITORY" "$WORKSPACE_DIR"
  else
    git clone --branch "$WORKSPACE_BASE_BRANCH" --single-branch "$WORKSPACE_REPOSITORY" "$WORKSPACE_DIR"
    git -C "$WORKSPACE_DIR" checkout -b "$WORKSPACE_BRANCH"
  fi
fi
rm -f "$CLAUDE_CONFIG_DIR/.credentials.json"
`

func buildStatefulSet(ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate, resourceName string) (*appsv1.StatefulSet, error) {
	requests, err := parseResourceList(tmpl.Spec.Resources.Requests)
	if err != nil {
		return nil, err
	}
	limits, err := parseResourceList(tmpl.Spec.Resources.Limits)
	if err != nil {
		return nil, err
	}

	baseBranch := ""
	if ws.Spec.BaseBranch != nil {
		baseBranch = *ws.Spec.BaseBranch
	}

	labels := workspaceLabels(ws)

	initEnv := []corev1.EnvVar{
		{Name: "WORKSPACE_DIR", Value: workingDirPath},
		{Name: "WORKSPACE_REPOSITORY", Value: ws.Spec.Repository},
		{Name: "WORKSPACE_BRANCH", Value: ws.Spec.Branch},
		{Name: "WORKSPACE_BASE_BRANCH", Value: baseBranch},
		{Name: "CLAUDE_CONFIG_DIR", Value: claudeConfigPath},
		{Name: "WORKSPACE_CACHE_DIR", Value: cacheDirPath},
	}

	// The supervisor derives the rest of the layout from the mount root, so a
	// path change stays in one place.
	workspaceEnv := []corev1.EnvVar{
		{Name: "WORKSPACE_MOUNT", Value: workspaceMountPath},
		{Name: "WORKSPACE_NAME", Value: ws.Name},
		{Name: "CLAUDE_AUTH_FILE", Value: authMountPath + "/credentials.json"},
	}
	// Left unset when the template does not pin one, so Claude Code applies its
	// own default rather than this controller inventing a model name (7.3).
	if tmpl.Spec.Model != "" {
		workspaceEnv = append(workspaceEnv, corev1.EnvVar{Name: "ANTHROPIC_MODEL", Value: tmpl.Spec.Model})
	}

	volumes := []corev1.Volume{
		{
			Name: workspaceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: resourceName},
			},
		},
		{
			Name: authSecretVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: tmpl.Spec.Auth.SecretRef},
			},
		},
	}
	volumeMounts := []corev1.VolumeMount{
		{Name: workspaceVolumeName, MountPath: workspaceMountPath},
		{Name: authSecretVolumeName, MountPath: authMountPath, ReadOnly: true},
	}

	replicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: ws.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: resourceName,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector:      map[string]string{"kubernetes.io/hostname": tmpl.Spec.NodeName},
					PriorityClassName: tmpl.Spec.PriorityClassName,
					InitContainers: []corev1.Container{
						{
							Name:         "workspace-init",
							Image:        tmpl.Spec.Image,
							Command:      []string{"sh", "-c", workspaceInitScript},
							Env:          initEnv,
							VolumeMounts: volumeMounts,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "workspace",
							Image:   tmpl.Spec.Image,
							Command: []string{supervisorBinaryPath},
							Env:     workspaceEnv,
							Ports: []corev1.ContainerPort{
								{Name: "supervisor", ContainerPort: supervisorPort},
							},
							Resources: corev1.ResourceRequirements{
								Requests: requests,
								Limits:   limits,
							},
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}
	return sts, nil
}
