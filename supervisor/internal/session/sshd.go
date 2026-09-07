package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Defaults match the OpenSSH layout of the workspace base image
// (apps/devplatform/image/workspace/Dockerfile). The authority key is not part
// of that image — it names a cluster's own certificate authority, so the base
// image would stop being usable anywhere else — and arrives as a Secret the
// controller mounts (resources.go sshCAMountPath).
const (
	DefaultSSHDBinary   = "/usr/sbin/sshd"
	DefaultKeygenBinary = "/usr/bin/ssh-keygen"
	DefaultCAPublicKey  = "/run/devplatform/ssh-ca/ca.pub"
	DefaultSSHPort      = 22

	// sshStateDirName sits beside the config and cache directories under the
	// PVC mount root rather than inside the checkout: the host key has to
	// survive a restart, and anything inside the checkout would show up as an
	// untracked change in the branch.
	sshStateDirName = ".ssh-endpoint"

	hostKeyName     = "ssh_host_ed25519_key"
	principalsName  = "authorized_principals"
	sshdConfigName  = "sshd_config"
	privilegeSepDir = "/run/sshd"
)

// SSHStateDir is where the endpoint keeps its host key and generated config.
func SSHStateDir(mountRoot string) string { return filepath.Join(mountRoot, sshStateDirName) }

// SSHEndpoint is the workspace's sshd, the optional route a developer's IDE
// takes into the working directory and the session (4.7). It authenticates
// nothing but certificates the shared gateway's authority signed, and accepts
// only the principal naming this workspace, so a certificate issued for one
// workspace cannot open another (4.11 / 4.12).
type SSHEndpoint struct {
	WorkspaceID string
	StateDir    string
	CAPublicKey string
	Port        int

	// Binary and KeygenBinary exist so the tests can stand in for OpenSSH.
	Binary       string
	KeygenBinary string
}

// Prepare writes the host key, the principals file and the config, and returns
// the config path.
func (e *SSHEndpoint) Prepare() (string, error) {
	if e.WorkspaceID == "" {
		return "", fmt.Errorf("session: ssh endpoint without a workspace identifier")
	}
	caKey := e.caPublicKey()
	if _, err := os.Stat(caKey); err != nil {
		return "", fmt.Errorf("session: ssh certificate authority key: %w", err)
	}
	if err := os.MkdirAll(e.StateDir, 0o700); err != nil {
		return "", err
	}
	// sshd refuses to start without its privilege separation directory, and a
	// container's /run is empty on every start.
	if err := os.MkdirAll(privilegeSepDir, 0o755); err != nil && !os.IsPermission(err) {
		return "", err
	}
	if err := e.ensureHostKey(); err != nil {
		return "", err
	}

	principals := filepath.Join(e.StateDir, principalsName)
	if err := os.WriteFile(principals, []byte(e.WorkspaceID+"\n"), 0o600); err != nil {
		return "", err
	}

	configPath := filepath.Join(e.StateDir, sshdConfigName)
	if err := os.WriteFile(configPath, []byte(e.config(caKey, principals)), 0o600); err != nil {
		return "", err
	}
	return configPath, nil
}

// Run serves in the foreground until ctx is cancelled.
func (e *SSHEndpoint) Run(ctx context.Context) error {
	configPath, err := e.Prepare()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, e.binary(), "-D", "-e", "-f", configPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// config is generated rather than baked into the image because the principal
// is only known once the workspace is provisioned, and both halves of the
// authentication rule have to be stated in the same place to stay in step.
func (e *SSHEndpoint) config(caKey, principals string) string {
	port := e.Port
	if port == 0 {
		port = DefaultSSHPort
	}
	// Root is the session's own user: the tmux server, Claude Code and the
	// checkout all belong to it, so an IDE arriving as anyone else would see a
	// different session. Reaching sshd at all already requires a certificate
	// for this workspace, which is where the access decision is made.
	return "Port " + strconv.Itoa(port) + "\n" +
		"HostKey " + filepath.Join(e.StateDir, hostKeyName) + "\n" +
		"TrustedUserCAKeys " + caKey + "\n" +
		"AuthorizedPrincipalsFile " + principals + "\n" +
		"AuthorizedKeysFile none\n" +
		"AuthorizedKeysCommand none\n" +
		"PubkeyAuthentication yes\n" +
		"PasswordAuthentication no\n" +
		"KbdInteractiveAuthentication no\n" +
		"PermitEmptyPasswords no\n" +
		"PermitRootLogin prohibit-password\n" +
		"UsePAM no\n" +
		"AllowTcpForwarding no\n" +
		"AllowAgentForwarding no\n" +
		"X11Forwarding no\n" +
		"PrintMotd no\n" +
		"Subsystem sftp internal-sftp\n"
}

func (e *SSHEndpoint) ensureHostKey() error {
	path := filepath.Join(e.StateDir, hostKeyName)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	keygen := e.KeygenBinary
	if keygen == "" {
		keygen = DefaultKeygenBinary
	}
	out, err := exec.Command(keygen, "-f", path, "-t", "ed25519", "-N", "", "-q").CombinedOutput()
	if err != nil {
		return fmt.Errorf("session: generate ssh host key: %w: %s", err, out)
	}
	return nil
}

func (e *SSHEndpoint) caPublicKey() string {
	if e.CAPublicKey == "" {
		return DefaultCAPublicKey
	}
	return e.CAPublicKey
}

func (e *SSHEndpoint) binary() string {
	if e.Binary == "" {
		return DefaultSSHDBinary
	}
	return e.Binary
}
