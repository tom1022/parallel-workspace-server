// Package routing provisions the externally reachable entry points for a
// workspace's preview and report endpoints (design.md "Routing Adapter",
// Requirement 3.1/3.2). The Workspace Controller depends only on the
// RoutingAdapter interface below; it never learns whether the implementation
// selected at deploy time generates a standard Kubernetes Ingress or a
// Traefik IngressRoute.
package routing

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// RoutingAdapter provisions the externally reachable entry points for one
// workspace. Implementations own the resource kind; callers never see it.
//
// Ensure is idempotent: calling it repeatedly for the same RoutingTarget
// converges on the same generated resources instead of erroring or
// duplicating them. After Ensure returns nil, the resources needed to reach
// target.Hostnames exist; after Remove returns nil, they don't.
type RoutingAdapter interface {
	Ensure(ctx context.Context, target RoutingTarget) error
	Remove(ctx context.Context, target RoutingTarget) error
}

// RoutingTarget describes one workspace's preview and report entry points.
// Hostnames must be non-empty; callers are responsible for that.
type RoutingTarget struct {
	// WorkspaceName and WorkspaceUID identify the owning Workspace, so
	// implementations can set an OwnerReference back to it (generated
	// resources are garbage-collected when the Workspace is deleted) without
	// a round trip to fetch the object the caller already has loaded.
	WorkspaceName string
	WorkspaceUID  types.UID
	Namespace     string
	// ServiceName is the backend Service the generated entry points forward
	// to, and the base name Remove uses to find them again — Remove reads
	// only Namespace and ServiceName, so a caller that only needs to delete
	// pre-existing resources (e.g. a rollback before the Workspace itself is
	// deleted) may leave Hostnames/Ports/Exposure zero.
	ServiceName string
	Hostnames   RoutingHostnames
	Ports       RoutingPorts

	// Labels are applied to every resource generated for this target,
	// matching the labels the reconciler already stamps on the rest of a
	// workspace's owned substrate. No query in this package depends on
	// them — they are a pass-through so the routing adapter's output stays
	// consistent with that convention without this package importing it.
	Labels map[string]string

	// Exposure carries what the operator configured for the preview and
	// report entry points. The platform does not protect them itself.
	Exposure RoutingExposure
}

// RoutingHostnames are the single-label hostnames (no domain suffix — each
// adapter's own configuration supplies that) the preview and report entry
// points are reachable at.
type RoutingHostnames struct {
	Preview string
	Report  string
}

// RoutingPorts are the target Service's ports the preview and report entry
// points forward to.
type RoutingPorts struct {
	Preview int32
	Report  int32
}

// RoutingExposure carries operator-configured, implementation-agnostic
// metadata for the preview and report entry points. The platform does not
// interpret it: it is a pass-through for the operator's own protection or
// temporary-exposure mechanism (e.g. an external tunnel), since neither
// route is reachable through the gateway that would otherwise provide one.
type RoutingExposure struct {
	// Annotations are attached verbatim to the generated entry point, so an
	// external service can pick the route up without this package knowing
	// about it.
	Annotations map[string]string
	// MiddlewareRefs names request-path handlers the operator wants applied,
	// meaningful only to routing implementations that have the concept
	// (e.g. Traefik middlewares).
	MiddlewareRefs []string
}

// ownerStub builds the minimal Workspace object controllerutil.SetControllerReference
// needs: it only reads Name/Namespace/UID plus the scheme-resolved GVK, so a
// full Get of the real object is unnecessary.
func ownerStub(target RoutingTarget) *devplatformv1alpha1.Workspace {
	return &devplatformv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      target.WorkspaceName,
			Namespace: target.Namespace,
			UID:       target.WorkspaceUID,
		},
	}
}

// ensureCreated makes obj's creation idempotent: a caller that runs Ensure
// again after the object already exists must not error (RoutingAdapter's
// idempotence contract).
func ensureCreated(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// entrySuffixes name-suffix every generated resource by which entry point it
// backs; Remove uses the same list to find them again without needing the
// rest of a RoutingTarget.
var entrySuffixes = [2]string{"-preview", "-report"}

// entryPoint is one (suffix, host, port) tuple an implementation turns into
// a single generated resource.
type entryPoint struct {
	suffix string
	host   string
	port   int32
}

// ingressEntryPoints expands target into its two entry points, so both
// implementations iterate the same preview/report pair instead of each
// repeating the pairing.
func ingressEntryPoints(target RoutingTarget) [2]entryPoint {
	return [2]entryPoint{
		{suffix: entrySuffixes[0], host: target.Hostnames.Preview, port: target.Ports.Preview},
		{suffix: entrySuffixes[1], host: target.Hostnames.Report, port: target.Ports.Report},
	}
}
