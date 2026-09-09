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
	// so it stays independent per workspace and survives restarts (5.3).
	// cacheDirPath is the package cache the supervisor points every package
	// manager at, so a suspend/resume does not re-download (15.12). Both
	// mirror session.Paths in the supervisor module.
	claudeConfigPath = workspaceMountPath + "/.claude-config"
	cacheDirPath     = workspaceMountPath + "/.cache"

	workspaceVolumeName = "workspace"

	// The Blackboard's CLAUDE.md, delivered as a mount so the supervisor
	// decides when it reaches the session rather than kubelet doing (10.6).
	// The path is a constant for the same reason workspaceMountPath is: it
	// must not vary between workspaces.
	blackboardVolumeName = "blackboard"
	blackboardMountPath  = "/run/devplatform/blackboard"

	labelWorkspaceName = "workspace.tom1022.github.io/workspace"

	// supervisorBinaryPath and supervisorPort must match the workspace base
	// image (apps/devplatform/image/workspace/Dockerfile) and the Session
	// Supervisor's own default listen address.
	supervisorBinaryPath = "/usr/local/bin/supervisor"
	supervisorPort       = 8787

	// sshPort is the workspace's own sshd, which the supervisor starts and
	// which only the private network route reaches (4.7). It is not exposed
	// through an IngressRoute: certificates are the only credential it
	// accepts, and the browser route never uses it.
	sshPort = 22

	// The certificate authority the workspace's sshd trusts. Only the public
	// half reaches a workspace; the private key stays in the gateway's own
	// namespace (4.11). The Secret name and key mirror
	// templates/infisical-secret.yaml, and the mount is optional so a cluster
	// that has not supplied an authority keeps the browser route (4.3) and
	// loses only the IDE route.
	sshCAPublicKeySecretName = "devplatform-workspace-ssh-ca"
	sshCAPublicKeyKey        = "ca.pub"
	sshCAVolumeName          = "ssh-ca"
	sshCAMountPath           = "/run/devplatform/ssh-ca"

	localPathStorageClass = "local-path"

	// Key names inside the evacuation destination's credential Secret.
	evacuationAccessKeyKey = "access-key"
	evacuationSecretKeyKey = "secret-key"

	// Key inside spec.auth.secretRef holding the long-lived Claude Code
	// credential. Delivered as CLAUDE_CODE_OAUTH_TOKEN (an env var, not a
	// mounted file): Claude Code reads it directly and never rewrites it, so
	// there is no refreshed-file-on-disk problem to solve (5.1).
	authTokenKey = "token"

	// Key inside spec.gitCredentialSecretRef holding the git HTTPS token.
	// Delivered as GIT_CREDENTIAL_TOKEN (an env var), same rationale as
	// authTokenKey: git only ever needs it via GIT_ASKPASS, so there is no
	// file on disk that gets rewritten and needs re-mounting.
	gitCredentialTokenKey = "token"

	// gitAskpassPath is where the init script writes the askpass helper.
	// It sits on the workspace PVC but outside workingDirPath for the same
	// reason documented on workingDirPath: nothing under the git checkout
	// may hold a credential a `git add -A` could pick up.
	gitAskpassPath = workspaceMountPath + "/.git-askpass"

	// The branch role's TLS client certificate, which CNPG issues into
	// "<databaserole-name>-client-cert" (database_reconciler.go). Mounting it
	// also gates the Pod on the role actually existing, which is the trigger
	// DB Bootstrap needs (8.5) — kubelet holds the containers until the Secret
	// is there.
	databaseCertVolumeName = "database-client-cert"
	databaseCertMountPath  = "/run/devplatform/database"
	databaseCertSecretName = "-client-cert"
	databasePort           = "5432"

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
//
// The restore sits inside the fresh-clone branch: an empty PVC is what node
// loss looks like from here, and it is the only state where replaying the last
// evacuation cannot overwrite newer work (16.9). The supervisor refuses the
// replay a second time on its own, so the two guards agree.
const workspaceInitScript = `set -eu
mkdir -p "$WORKSPACE_DIR" "$CLAUDE_CONFIG_DIR" "$WORKSPACE_CACHE_DIR"
# ponytail: HTTPS token auth only, no SSH remote support — lift this ceiling
# only once a user actually needs an SSH git remote.
if [ -n "${GIT_CREDENTIAL_TOKEN:-}" ]; then
  printf '#!/bin/sh\necho "$GIT_CREDENTIAL_TOKEN"\n' > "$GIT_ASKPASS"
  chmod +x "$GIT_ASKPASS"
fi
rm -f "$WORKSPACE_DIR/.git/index.lock" "$WORKSPACE_DIR/.git/HEAD.lock" "$WORKSPACE_DIR/.git/shallow.lock"
if [ ! -d "$WORKSPACE_DIR/.git" ]; then
  if git ls-remote --exit-code --heads "$WORKSPACE_REPOSITORY" "$WORKSPACE_BRANCH" >/dev/null 2>&1; then
    git clone --branch "$WORKSPACE_BRANCH" --single-branch "$WORKSPACE_REPOSITORY" "$WORKSPACE_DIR"
  else
    git clone --branch "$WORKSPACE_BASE_BRANCH" --single-branch "$WORKSPACE_REPOSITORY" "$WORKSPACE_DIR"
    git -C "$WORKSPACE_DIR" checkout -b "$WORKSPACE_BRANCH"
  fi
  ` + supervisorBinaryPath + ` restore
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

	// The supervisor evacuates on its own after every turn and on request
	// before a stop (16.7/16.8) and replays the last snapshot from the init
	// container after node loss (16.9), so both containers address the same
	// destination. The credentials arrive by Secret reference rather than
	// value: the controller never reads them and they must not appear in the
	// Pod spec.
	evacuationEnv := []corev1.EnvVar{
		{Name: "WORKSPACE_ID", Value: resourceName},
		{Name: "EVACUATION_ENDPOINT", Value: tmpl.Spec.Evacuation.Endpoint},
		{Name: "EVACUATION_REGION", Value: tmpl.Spec.Evacuation.Region},
		{Name: "EVACUATION_BUCKET", Value: tmpl.Spec.Evacuation.Bucket},
		secretEnv("EVACUATION_ACCESS_KEY", tmpl.Spec.Evacuation.SecretRef, evacuationAccessKeyKey),
		secretEnv("EVACUATION_SECRET_KEY", tmpl.Spec.Evacuation.SecretRef, evacuationSecretKeyKey),
	}

	dbEnabled := tmpl.Spec.Database != nil
	databaseEnv := branchDatabaseEnv(ws, tmpl, resourceName)

	// Only set when the caller referenced a credential Secret (5.1-5.3):
	// with none, the workspace clones anonymously exactly as it did before
	// this env var existed, and an unset GIT_ASKPASS would break that.
	var gitCredentialEnv []corev1.EnvVar
	if ws.Spec.GitCredentialSecretRef != nil {
		gitCredentialEnv = []corev1.EnvVar{
			secretEnv("GIT_CREDENTIAL_TOKEN", *ws.Spec.GitCredentialSecretRef, gitCredentialTokenKey),
			{Name: "GIT_ASKPASS", Value: gitAskpassPath},
		}
	}

	initEnv := append([]corev1.EnvVar{
		{Name: "WORKSPACE_DIR", Value: workingDirPath},
		{Name: "WORKSPACE_REPOSITORY", Value: ws.Spec.Repository},
		{Name: "WORKSPACE_BRANCH", Value: ws.Spec.Branch},
		{Name: "WORKSPACE_BASE_BRANCH", Value: baseBranch},
		{Name: "CLAUDE_CONFIG_DIR", Value: claudeConfigPath},
		{Name: "WORKSPACE_CACHE_DIR", Value: cacheDirPath},
	}, evacuationEnv...)
	initEnv = append(initEnv, gitCredentialEnv...)

	// The supervisor derives the rest of the layout from the mount root, so a
	// path change stays in one place.
	workspaceEnv := append([]corev1.EnvVar{
		{Name: "WORKSPACE_MOUNT", Value: workspaceMountPath},
		{Name: "WORKSPACE_NAME", Value: ws.Name},
		secretEnv("CLAUDE_CODE_OAUTH_TOKEN", tmpl.Spec.Auth.SecretRef, authTokenKey),
		{Name: "SSH_CA_PUBLIC_KEY", Value: sshCAMountPath + "/" + sshCAPublicKeyKey},
		{Name: "BLACKBOARD_CLAUDE_MD", Value: blackboardMountPath + "/" + ClaudeMDKey},
	}, evacuationEnv...)
	// The init container needs this to clone; the long-running workspace
	// container needs it for any later git push/pull Claude Code performs.
	workspaceEnv = append(workspaceEnv, gitCredentialEnv...)
	// 8.8: every process started under the working directory inherits the
	// branch's own connection, so nothing in the repository has to be
	// configured for a per-branch database.
	workspaceEnv = append(workspaceEnv, databaseEnv...)

	// Left unset when neither the template nor a quota-driven switch pins one,
	// so Claude Code applies its own default rather than this controller
	// inventing a model name (7.3). The switched-to model wins: it is the
	// template's model plus the knowledge that its window is spent (7.7).
	model := tmpl.Spec.Model
	if active := ws.Annotations[AnnotationActiveModel]; active != "" {
		model = active
	}
	if model != "" {
		workspaceEnv = append(workspaceEnv, corev1.EnvVar{Name: "ANTHROPIC_MODEL", Value: model})
	}

	volumes := []corev1.Volume{
		{
			Name: workspaceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: resourceName},
			},
		},
		{
			Name: sshCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: sshCAPublicKeySecretName,
					Optional:   ptr(true),
				},
			},
		},
		{
			Name: blackboardVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: ClaudeMDConfigMapName(resourceName),
					},
					// Shared context is not a precondition for running the
					// branch: a document that has not been generated yet must
					// not hold the Pod at ContainerCreating.
					Optional: ptr(true),
				},
			},
		},
	}
	volumeMounts := []corev1.VolumeMount{
		{Name: workspaceVolumeName, MountPath: workspaceMountPath},
		{Name: sshCAVolumeName, MountPath: sshCAMountPath, ReadOnly: true},
		{Name: blackboardVolumeName, MountPath: blackboardMountPath, ReadOnly: true},
	}
	// The branch role's TLS client certificate only exists when a database
	// is configured for this template (Requirement 3.5); mounting it also
	// gates the Pod on the role actually existing (see databaseCertVolumeName's
	// doc comment), so it must not appear at all when there is no role to
	// wait on.
	if dbEnabled {
		volumes = append(volumes, corev1.Volume{
			Name: databaseCertVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: resourceName + databaseCertSecretName,
					// libpq refuses a private key readable by anyone but its
					// owner, and a Secret volume is 0644 by default.
					DefaultMode: ptr(int32(0o600)),
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: databaseCertVolumeName, MountPath: databaseCertMountPath, ReadOnly: true})
	}

	// 15.6: a namespace ResourceQuota that requires resources on every
	// container rejects the whole Pod if any initContainer omits them, so
	// both init containers carry the same requests/limits as the workspace
	// container rather than running unbounded.
	initResources := corev1.ResourceRequirements{Requests: requests, Limits: limits}
	initContainers := []corev1.Container{
		{
			Name:         "workspace-init",
			Image:        tmpl.Spec.Image,
			Command:      []string{"sh", "-c", workspaceInitScript},
			Env:          initEnv,
			VolumeMounts: volumeMounts,
			Resources:    initResources,
		},
	}
	if dbEnabled {
		// Second, because it reads the repository's own declaration of how
		// it migrates out of the checkout the container above produces, and
		// the schema has to be in place before the session is handed the
		// branch (8.5). Omitted entirely without a database (Requirement
		// 3.5): there is no schema to bootstrap.
		initContainers = append(initContainers, corev1.Container{
			Name:         "db-bootstrap",
			Image:        tmpl.Spec.Image,
			Command:      []string{supervisorBinaryPath, "dbboot"},
			Env:          append(append([]corev1.EnvVar{}, initEnv...), databaseEnv...),
			VolumeMounts: volumeMounts,
			Resources:    initResources,
		})
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
					InitContainers:    initContainers,
					Containers: []corev1.Container{
						{
							Name:    "workspace",
							Image:   tmpl.Spec.Image,
							Command: []string{supervisorBinaryPath},
							Env:     workspaceEnv,
							Ports: []corev1.ContainerPort{
								{Name: "supervisor", ContainerPort: supervisorPort},
								{Name: "ssh", ContainerPort: sshPort},
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

// branchDatabaseEnv is the branch's connection, in libpq's own variable names
// so a repository's migration tool and application pick it up without being
// told about this platform, plus the URL form the JavaScript ecosystem expects.
//
// The Database and DatabaseRole are created in the workspace's namespace
// (database_reconciler.go), so the instance serving them is addressed there too.
//
// ponytail: sslmode=require, not verify-full — the client certificate still
// authenticates us to the server, and verifying the server in turn would mean
// mounting the cluster CA as a second Secret for an in-cluster hop.
func branchDatabaseEnv(ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate, resourceName string) []corev1.EnvVar {
	// nil means this template provisions workspaces without a branch
	// database at all (Requirement 3.5) — no connection to describe.
	if tmpl.Spec.Database == nil {
		return nil
	}
	host := fmt.Sprintf("%s-rw.%s.svc.cluster.local", tmpl.Spec.Database.ClusterRef, ws.Namespace)
	cert := databaseCertMountPath + "/tls.crt"
	key := databaseCertMountPath + "/tls.key"
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: host},
		{Name: "PGPORT", Value: databasePort},
		{Name: "PGDATABASE", Value: resourceName},
		{Name: "PGUSER", Value: resourceName},
		{Name: "PGSSLMODE", Value: "require"},
		{Name: "PGSSLCERT", Value: cert},
		{Name: "PGSSLKEY", Value: key},
		{Name: "DATABASE_URL", Value: fmt.Sprintf(
			"postgresql://%s@%s:%s/%s?sslmode=require&sslcert=%s&sslkey=%s",
			resourceName, host, databasePort, resourceName, cert, key)},
	}
}

func ptr[T any](v T) *T { return &v }

func secretEnv(name, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	}
}
