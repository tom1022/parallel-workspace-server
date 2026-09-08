package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/tom1022/gitops-apps/apps/devplatform/gateway/internal/gateway"
)

// gatewayVersion is set at build time via -ldflags "-X main.gatewayVersion=...".
// Empty means "unset" — GET /version passes that through rather than
// inventing a value, since a client is expected to hold off judging
// compatibility against a gateway that never set one.
var gatewayVersion string

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("gateway: kubernetes config: %v", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("gateway: kubernetes client: %v", err)
	}

	namespace := os.Getenv("GATEWAY_NAMESPACE")

	// A CA that cannot be established leaves certificate issuance answering 503
	// while the browser path keeps working; refusing to start would take every
	// workspace offline over an optional capability.
	ca, err := (&gateway.CABootstrap{
		Dynamic:             client,
		Namespace:           namespace,
		SecretName:          env("SSH_CA_SECRET_NAME", "devplatform-gateway-ssh-ca"),
		KeyName:             env("SSH_CA_SECRET_KEY", "id_ca"),
		PublicKeyNamespace:  env("WORKSPACE_NAMESPACE", "devplatform-workspaces"),
		PublicKeySecretName: env("SSH_CA_PUBLIC_SECRET_NAME", "devplatform-workspace-ssh-ca"),
		PublicKeyName:       env("SSH_CA_PUBLIC_SECRET_KEY", "ca.pub"),
		TTL:                 duration("SSH_CERTIFICATE_TTL", gateway.DefaultCertificateTTL),
	}).Ensure(context.Background())
	if err != nil {
		log.Printf("gateway: ssh certificate issuance disabled: %v", err)
	}

	verifier := &gateway.Verifier{
		Issuer:   os.Getenv("OIDC_ISSUER"),
		Audience: os.Getenv("OIDC_AUDIENCE"),
		JWKSURL:  os.Getenv("OIDC_JWKS_URL"),
	}

	// Every route this gateway serves has to be identified (4.1), so at
	// least one Authenticator must end up configured: the external IdP when
	// an operator points one at this gateway, the gateway's own User Store
	// unless GATEWAY_LOCAL_AUTH_ENABLED=false stops it, or both together.
	var authenticators []gateway.Authenticator
	idpConfigured := verifier.Issuer != "" && verifier.Audience != "" && verifier.JWKSURL != ""
	if idpConfigured {
		authenticators = append(authenticators, &gateway.IDPAuthenticator{Verifier: verifier})
	}

	localAuthEnabled := env("GATEWAY_LOCAL_AUTH_ENABLED", "true") == "true"
	var local *gateway.LocalAuth
	if localAuthEnabled {
		users := &gateway.UserStore{
			Dynamic:    client,
			Namespace:  namespace,
			SecretName: env("USER_STORE_SECRET_NAME", "devplatform-gateway-users"),
		}
		result, err := (&gateway.IdentityBootstrap{
			Users:                users,
			Dynamic:              client,
			Namespace:            namespace,
			AdminUsername:        env("GATEWAY_ADMIN_USERNAME", gateway.DefaultAdminUsername),
			CredentialSecretName: env("INITIAL_CREDENTIAL_SECRET_NAME", "devplatform-gateway-initial-credential"),
			SessionKeySecretName: env("SESSION_KEY_SECRET_NAME", "devplatform-gateway-session-key"),
		}).Ensure(context.Background())
		if err != nil {
			// Unlike the SSH CA, local authentication is the only identity
			// mechanism a fresh deployment has: failing to seed it here
			// would silently start the gateway with no working login path.
			log.Fatalf("gateway: identity bootstrap: %v", err)
		}

		issuer := &gateway.JWTSessionIssuer{Key: result.SessionKey, Users: users}
		authenticators = append(authenticators, &gateway.LocalAuthenticator{Issuer: issuer})
		local = &gateway.LocalAuth{
			Users:      users,
			Sessions:   issuer,
			SessionTTL: duration("SESSION_TTL", gateway.DefaultSessionTTL),
		}
	}

	if len(authenticators) == 0 {
		log.Fatal("gateway: no authenticator configured — set OIDC_ISSUER/OIDC_AUDIENCE/OIDC_JWKS_URL or leave GATEWAY_LOCAL_AUTH_ENABLED at its default")
	}

	handler := &gateway.Handler{
		Store: &gateway.Store{
			Dynamic:            client,
			Namespace:          env("WORKSPACE_NAMESPACE", "devplatform-workspaces"),
			DefaultTemplateRef: env("WORKSPACE_TEMPLATE_REF", "default"),
		},
		Verifier: verifier,
		Identity: &gateway.Identity{Authenticators: authenticators},
		CA:       ca,
		Local:    local,
		Version:  gatewayVersion,
	}

	addr := env("GATEWAY_ADDR", ":8080")
	log.Printf("gateway: listening on %s", addr)
	server := &http.Server{Addr: addr, Handler: handler.Routes()}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("gateway: %v", err)
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func duration(name string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return v
}
