package routing

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	traefikAPIVersion = "traefik.io/v1alpha1"
	traefikKind       = "IngressRoute"

	// traefikEntryPoint is Traefik's own well-known name for its HTTPS
	// listener, not an environment-specific identifier, so it stays a
	// constant rather than moving to configuration.
	traefikEntryPoint = "websecure"
)

// TraefikAdapter generates one Traefik IngressRoute per entry point. A
// cluster that already runs Traefik (e.g. k3s' bundled instance) can select
// it instead of IngressAdapter (Requirement 3.2).
type TraefikAdapter struct {
	Client client.Client
	Scheme *runtime.Scheme

	// Domain is appended to each RoutingHostnames label to build the route's
	// Host(`...`) match.
	Domain string
	// TLSSecretName is the Secret backing Domain's certificate.
	TLSSecretName string
}

var _ RoutingAdapter = (*TraefikAdapter)(nil)

// TraefikIngressRouteRef returns an empty IngressRoute reference suitable
// for client.Get/Delete by namespace/name. Exported so callers (the
// Workspace Controller's rollback path, tests) can look a generated
// IngressRoute up without this package exposing anything richer than "here
// is the kind and name" — they never construct or read its spec.
func TraefikIngressRouteRef(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(traefikAPIVersion)
	u.SetKind(traefikKind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func (a *TraefikAdapter) Ensure(ctx context.Context, target RoutingTarget) error {
	for _, ep := range ingressEntryPoints(target) {
		ir := a.build(target, ep)
		if err := controllerutil.SetControllerReference(ownerStub(target), ir, a.Scheme); err != nil {
			return err
		}
		if err := ensureCreated(ctx, a.Client, ir); err != nil {
			return fmt.Errorf("ensure IngressRoute %s: %w", ir.GetName(), err)
		}
	}
	return nil
}

func (a *TraefikAdapter) Remove(ctx context.Context, target RoutingTarget) error {
	for _, suffix := range entrySuffixes {
		err := a.Client.Delete(ctx, TraefikIngressRouteRef(target.Namespace, target.ServiceName+suffix))
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (a *TraefikAdapter) build(target RoutingTarget, ep entryPoint) *unstructured.Unstructured {
	ir := TraefikIngressRouteRef(target.Namespace, target.ServiceName+ep.suffix)
	if len(target.Labels) > 0 {
		ir.SetLabels(target.Labels)
	}
	if len(target.Exposure.Annotations) > 0 {
		ir.SetAnnotations(target.Exposure.Annotations)
	}

	route := map[string]interface{}{
		"kind":  "Rule",
		"match": "Host(`" + ep.host + "." + a.Domain + "`)",
		"services": []interface{}{
			map[string]interface{}{
				"name": target.ServiceName,
				"port": int64(ep.port),
			},
		},
	}
	if len(target.Exposure.MiddlewareRefs) > 0 {
		middlewares := make([]interface{}, 0, len(target.Exposure.MiddlewareRefs))
		for _, name := range target.Exposure.MiddlewareRefs {
			middlewares = append(middlewares, map[string]interface{}{"name": name})
		}
		route["middlewares"] = middlewares
	}

	ir.Object["spec"] = map[string]interface{}{
		"entryPoints": []interface{}{traefikEntryPoint},
		"routes":      []interface{}{route},
		"tls": map[string]interface{}{
			"secretName": a.TLSSecretName,
		},
	}
	return ir
}
