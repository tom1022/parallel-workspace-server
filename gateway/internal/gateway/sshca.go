package gateway

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
)

// DefaultCertificateTTL keeps an issued certificate short-lived so that
// revocation never has to be managed: a certificate that outlives its session
// window is the only thing a compromised client could reuse (4.11).
const DefaultCertificateTTL = 5 * time.Minute

// IssuedCertificate is the response body of POST /api/ssh/certificate.
type IssuedCertificate struct {
	Certificate string    `json:"certificate"`
	Principal   string    `json:"principal"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// CertificateAuthority signs the short-lived user certificates that let a
// developer's IDE reach one workspace over SSH. The workspace identifier is the
// certificate's sole principal, and the workspace's sshd requires the principal
// to match its own identifier, which is what confines a certificate to the
// workspace it was issued for (4.11).
type CertificateAuthority struct {
	Signer ssh.Signer
	// TTL is the certificate lifetime. Zero means DefaultCertificateTTL.
	TTL time.Duration
}

// Issue signs publicKey (an authorized_keys line) for workspaceID.
func (ca *CertificateAuthority) Issue(workspaceID, publicKey string) (IssuedCertificate, error) {
	if ca == nil || ca.Signer == nil {
		return IssuedCertificate{}, errors.New("gateway: no SSH certificate authority configured")
	}
	if workspaceID == "" {
		return IssuedCertificate{}, errors.New("gateway: no workspace identifier")
	}
	if publicKey == "" {
		return IssuedCertificate{}, errors.New("gateway: no public key")
	}
	userKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("gateway: parse public key: %w", err)
	}

	ttl := ca.TTL
	if ttl <= 0 {
		ttl = DefaultCertificateTTL
	}
	now := time.Now()
	expiry := now.Add(ttl)

	serial, err := randomSerial()
	if err != nil {
		return IssuedCertificate{}, err
	}

	cert := &ssh.Certificate{
		Key:             userKey,
		Serial:          serial,
		CertType:        ssh.UserCert,
		KeyId:           workspaceID,
		ValidPrincipals: []string{workspaceID},
		// A minute of leeway absorbs clock skew between the gateway and the
		// workspace without meaningfully lengthening the certificate's reach.
		ValidAfter:  uint64(now.Add(-time.Minute).Unix()),
		ValidBefore: uint64(expiry.Unix()),
		Permissions: ssh.Permissions{
			// Only a pty is granted. Port and agent forwarding would turn a
			// workspace certificate into a pivot into the cluster network,
			// which the workspace NetworkPolicy is there to prevent.
			Extensions: map[string]string{"permit-pty": ""},
		},
	}
	if err := cert.SignCert(rand.Reader, ca.Signer); err != nil {
		return IssuedCertificate{}, fmt.Errorf("gateway: sign certificate: %w", err)
	}

	return IssuedCertificate{
		Certificate: string(ssh.MarshalAuthorizedKey(cert)),
		Principal:   workspaceID,
		ExpiresAt:   expiry,
	}, nil
}

func randomSerial() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("gateway: certificate serial: %w", err)
	}
	return binary.BigEndian.Uint64(b[:]), nil
}
