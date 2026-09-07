package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEndpoint(t *testing.T) *SSHEndpoint {
	t.Helper()
	dir := t.TempDir()
	caKey := filepath.Join(dir, "ca.pub")
	if err := os.WriteFile(caKey, []byte("ssh-ed25519 AAAA ca\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &SSHEndpoint{
		WorkspaceID:  "ws-feature-login",
		StateDir:     filepath.Join(dir, "ssh"),
		CAPublicKey:  caKey,
		Port:         22,
		KeygenBinary: stubBinary(t, dir, "ssh-keygen"),
		Binary:       stubBinary(t, dir, "sshd"),
	}
}

// stubBinary stands in for the OpenSSH binaries the base image ships but the
// test host need not have. It records its arguments and, when asked to write a
// key, creates the file at the path following -f.
func stubBinary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> \"" + filepath.Join(dir, name+".calls") + "\"\n" +
		"if [ \"$1\" = \"-f\" ]; then : > \"$2\"; fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func calls(t *testing.T, endpoint *SSHEndpoint, name string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(filepath.Dir(endpoint.CAPublicKey), name+".calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

// 4.12: the certificate authority is the only thing sshd trusts. Password
// authentication, keyboard-interactive and plain public keys are all off, and
// there is no authorized_keys path for anything to write into.
func TestSSHEndpoint_TrustsOnlyTheCertificateAuthority(t *testing.T) {
	endpoint := testEndpoint(t)

	configPath, err := endpoint.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := string(body)

	for _, want := range []string{
		"TrustedUserCAKeys " + endpoint.CAPublicKey,
		"AuthorizedKeysFile none",
		"AuthorizedKeysCommand none",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"PermitEmptyPasswords no",
		"PubkeyAuthentication yes",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("sshd config is missing %q:\n%s", want, config)
		}
	}
}

// 4.11: the certificate's principal is the workspace identifier, and sshd
// accepts exactly that one, which is what stops a certificate issued for one
// workspace from opening another.
func TestSSHEndpoint_AcceptsOnlyItsOwnWorkspaceAsPrincipal(t *testing.T) {
	endpoint := testEndpoint(t)

	configPath, err := endpoint.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	principalsPath := filepath.Join(endpoint.StateDir, "authorized_principals")
	if !strings.Contains(string(config), "AuthorizedPrincipalsFile "+principalsPath) {
		t.Fatalf("sshd config does not point at the principals file:\n%s", config)
	}
	principals, err := os.ReadFile(principalsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(principals) != endpoint.WorkspaceID+"\n" {
		t.Errorf("principals = %q, want only this workspace's identifier", principals)
	}
}

// A workspace with no identity would accept every certificate the CA ever
// signed, so it must not come up at all.
func TestSSHEndpoint_RefusesToStartWithoutAWorkspaceIdentifier(t *testing.T) {
	endpoint := testEndpoint(t)
	endpoint.WorkspaceID = ""

	if _, err := endpoint.Prepare(); err == nil {
		t.Fatal("Prepare() accepted an empty workspace identifier")
	}
}

// Without the CA public key sshd would trust nothing and every connection
// would fail authentication; refusing here says why.
func TestSSHEndpoint_RefusesToStartWithoutTheAuthorityKey(t *testing.T) {
	endpoint := testEndpoint(t)
	endpoint.CAPublicKey = filepath.Join(endpoint.StateDir, "absent.pub")

	if _, err := endpoint.Prepare(); err == nil {
		t.Fatal("Prepare() accepted a missing certificate authority key")
	}
}

// The host key lives on the workspace volume: a restart or a suspend/resume
// that presented a new one would look like an interception to the IDE and be
// refused.
func TestSSHEndpoint_ReusesTheHostKeyAcrossRestarts(t *testing.T) {
	endpoint := testEndpoint(t)

	if _, err := endpoint.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := endpoint.Prepare(); err != nil {
		t.Fatal(err)
	}

	if got := calls(t, endpoint, "ssh-keygen"); len(got) != 1 {
		t.Errorf("ssh-keygen ran %d times, want once: %v", len(got), got)
	}
	if _, err := os.Stat(filepath.Join(endpoint.StateDir, "ssh_host_ed25519_key")); err != nil {
		t.Errorf("host key was not generated: %v", err)
	}
}

func TestSSHEndpoint_RunServesInTheForegroundWithTheGeneratedConfig(t *testing.T) {
	endpoint := testEndpoint(t)

	if err := endpoint.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := calls(t, endpoint, "sshd")
	if len(got) != 1 {
		t.Fatalf("sshd ran %d times, want once: %v", len(got), got)
	}
	if want := "-D -e -f " + filepath.Join(endpoint.StateDir, "sshd_config"); got[0] != want {
		t.Errorf("sshd args = %q, want %q", got[0], want)
	}
}
