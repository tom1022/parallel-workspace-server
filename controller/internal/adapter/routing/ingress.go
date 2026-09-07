package routing

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// IngressAdapter generates one standard Kubernetes Ingress per entry point.
// It requires no vendor-specific ingress controller CRD — Requirement 3.1:
// a workspace stays externally reachable with just the standard Ingress
// definition.
type IngressAdapter struct {
	Client client.Client
	Scheme *runtime.Scheme

	// Domain is appended to each RoutingHostnames label to build the rule's
	// host.
	Domain string
	// TLSSecretName is the Secret backing Domain's certificate.
	TLSSecretName string
	// ClassName selects spec.ingressClassName. Empty defers to the
	// cluster's default IngressClass.
	ClassName string
}

var _ RoutingAdapter = (*IngressAdapter)(nil)

func (a *IngressAdapter) Ensure(ctx context.Context, target RoutingTarget) error {
	for _, ep := range ingressEntryPoints(target) {
		ing := a.build(target, ep)
		if err := controllerutil.SetControllerReference(ownerStub(target), ing, a.Scheme); err != nil {
			return err
		}
		if err := ensureCreated(ctx, a.Client, ing); err != nil {
			return fmt.Errorf("ensure Ingress %s: %w", ing.Name, err)
		}
	}
	return nil
}

func (a *IngressAdapter) Remove(ctx context.Context, target RoutingTarget) error {
	for _, suffix := range entrySuffixes {
		err := a.Client.Delete(ctx, &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: target.ServiceName + suffix, Namespace: target.Namespace},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (a *IngressAdapter) build(target RoutingTarget, ep entryPoint) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	fqdn := ep.host + "." + a.Domain
	var className *string
	if a.ClassName != "" {
		className = &a.ClassName
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        target.ServiceName + ep.suffix,
			Namespace:   target.Namespace,
			Labels:      target.Labels,
			Annotations: target.Exposure.Annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: className,
			TLS: []networkingv1.IngressTLS{
				{Hosts: []string{fqdn}, SecretName: a.TLSSecretName},
			},
			Rules: []networkingv1.IngressRule{
				{
					Host: fqdn,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: target.ServiceName,
											Port: networkingv1.ServiceBackendPort{Number: ep.port},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
