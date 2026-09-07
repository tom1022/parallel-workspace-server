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

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("gateway: kubernetes config: %v", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("gateway: kubernetes client: %v", err)
	}

	// A CA that cannot be established leaves certificate issuance answering 503
	// while the browser path keeps working; refusing to start would take every
	// workspace offline over an optional capability.
	ca, err := (&gateway.CABootstrap{
		Dynamic:             client,
		Namespace:           os.Getenv("GATEWAY_NAMESPACE"),
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

	handler := &gateway.Handler{
		Store: &gateway.Store{
			Dynamic:            client,
			Namespace:          env("WORKSPACE_NAMESPACE", "devplatform-workspaces"),
			DefaultTemplateRef: env("WORKSPACE_TEMPLATE_REF", "default"),
		},
		Verifier: &gateway.Verifier{
			Issuer:   os.Getenv("OIDC_ISSUER"),
			Audience: os.Getenv("OIDC_AUDIENCE"),
			JWKSURL:  os.Getenv("OIDC_JWKS_URL"),
		},
		CA: ca,
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
