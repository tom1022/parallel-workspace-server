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
	// workingDirMountPath is where the branch's git working directory (and, by
	// extension, the CLAUDE_CONFIG_DIR below it) lives inside the workspace PVC.
	workingDirMountPath = "/workspace"

	// claudeConfigDirName keeps Claude Code's config area under the per-workspace
	// PVC so it stays independent per workspace and survives restarts (5.3),
	// distinct from the read-only mounted long-lived credential in authMountPath.
	claudeConfigDirName  = ".claude-config"
	authMountPath        = "/run/devplatform/claude-auth"
	authSecretVolumeName = "claude-auth"
	workspaceVolumeName  = "workspace"

	labelWorkspaceName = "devplatform.fickledev.com/workspace"

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
rm -f "$WORKSPACE_DIR/.git/index.lock" "$WORKSPACE_DIR/.git/HEAD.lock" "$WORKSPACE_DIR/.git/shallow.lock"
if [ ! -d "$WORKSPACE_DIR/.git" ]; then
  if git ls-remote --exit-code --heads "$WORKSPACE_REPOSITORY" "$WORKSPACE_BRANCH" >/dev/null 2>&1; then
    git clone --branch "$WORKSPACE_BRANCH" --single-branch "$WORKSPACE_REPOSITORY" "$WORKSPACE_DIR"
  else
    git clone --branch "$WORKSPACE_BASE_BRANCH" --single-branch "$WORKSPACE_REPOSITORY" "$WORKSPACE_DIR"
    git -C "$WORKSPACE_DIR" checkout -b "$WORKSPACE_BRANCH"
  fi
fi
mkdir -p "$CLAUDE_CONFIG_DIR"
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
	claudeConfigDir := workingDirMountPath + "/" + claudeConfigDirName

	initEnv := []corev1.EnvVar{
		{Name: "WORKSPACE_DIR", Value: workingDirMountPath},
		{Name: "WORKSPACE_REPOSITORY", Value: ws.Spec.Repository},
		{Name: "WORKSPACE_BRANCH", Value: ws.Spec.Branch},
		{Name: "WORKSPACE_BASE_BRANCH", Value: baseBranch},
		{Name: "CLAUDE_CONFIG_DIR", Value: claudeConfigDir},
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
		{Name: workspaceVolumeName, MountPath: workingDirMountPath},
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
							// Placeholder entrypoint: Session Supervisor (task 3) replaces this
							// with the resident Claude Code session process.
							Name:    "workspace",
							Image:   tmpl.Spec.Image,
							Command: []string{"sleep", "infinity"},
							Env: []corev1.EnvVar{
								{Name: "CLAUDE_CONFIG_DIR", Value: claudeConfigDir},
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
