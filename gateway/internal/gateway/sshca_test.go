package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newTestCA(t *testing.T) (*CertificateAuthority, ssh.PublicKey) {
	t.Helper()
	_, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(caPriv)
	if err != nil {
		t.Fatal(err)
	}
	return &CertificateAuthority{Signer: signer, TTL: 5 * time.Minute}, signer.PublicKey()
}

func newUserKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(sshPub))
}

func TestIssueBindsCertificateToOneWorkspace(t *testing.T) {
	ca, caPub := newTestCA(t)

	issued, err := ca.Issue("branch-a", newUserKey(t))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(issued.Certificate))
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}
	cert, ok := parsed.(*ssh.Certificate)
	if !ok {
		t.Fatalf("issued key is %T, want *ssh.Certificate", parsed)
	}

	if cert.CertType != ssh.UserCert {
		t.Errorf("CertType = %d, want UserCert", cert.CertType)
	}
	if len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != "branch-a" {
		t.Errorf("ValidPrincipals = %v, want [branch-a]", cert.ValidPrincipals)
	}

	// The regression this guards: a certificate minted for one workspace must
	// not authenticate against another (4.11).
	checker := &ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool {
		return string(k.Marshal()) == string(caPub.Marshal())
	}}
	if err := checker.CheckCert("branch-a", cert); err != nil {
		t.Errorf("CheckCert(branch-a): %v", err)
	}
	if err := checker.CheckCert("branch-b", cert); err == nil {
		t.Error("CheckCert(branch-b) succeeded, want rejection")
	}
}

func TestIssueIsShortLived(t *testing.T) {
	ca, _ := newTestCA(t)
	issued, err := ca.Issue("branch-a", newUserKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(issued.ExpiresAt); d <= 0 || d > 5*time.Minute+time.Minute {
		t.Fatalf("certificate valid for %v, want within the configured 5m TTL", d)
	}
}

func TestIssueRejectsUnusableInput(t *testing.T) {
	ca, _ := newTestCA(t)
	for name, tc := range map[string]struct{ workspace, key string }{
		"no workspace":  {"", newUserKey(t)},
		"no key":        {"branch-a", ""},
		"malformed key": {"branch-a", "ssh-ed25519 not-base64"},
	} {
		if _, err := ca.Issue(tc.workspace, tc.key); err == nil {
			t.Errorf("%s: Issue succeeded, want error", name)
		}
	}
}

func TestUnconfiguredCAReportsUnavailable(t *testing.T) {
	var ca CertificateAuthority
	if _, err := ca.Issue("branch-a", newUserKey(t)); err == nil {
		t.Fatal("Issue succeeded without a signing key, want error")
	}
}
