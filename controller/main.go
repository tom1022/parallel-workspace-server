// Command controller runs the Workspace Controller: the single writer of
// Workspace/WorkspaceTemplate-owned substrate inside the workspace-dedicated
// namespace (see internal/controller for the reconciliation logic).
package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/adapter/database"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/adapter/routing"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/controller"
)

func main() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("setup")

	workspaceNamespace := os.Getenv("WORKSPACE_NAMESPACE")
	if workspaceNamespace == "" {
		log.Error(nil, "WORKSPACE_NAMESPACE must be set; the controller must not watch or operate outside the workspace-dedicated namespace")
		os.Exit(1)
	}

	if err := devplatformv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		log.Error(err, "unable to add devplatform types to scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme.Scheme,
		// The cache (and therefore every watch/list/get the reconciler makes
		// through the manager's client) is scoped to this one namespace, so the
		// controller has no way to observe or touch cluster resources outside it
		// (design.md: "ワークスペース専用ネームスペース外のリソースを操作しない").
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				workspaceNamespace: {},
			},
		},
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	routingAdapter, routingDomain, routingExposure := buildRoutingAdapter(log, mgr.GetClient(), mgr.GetScheme())
	databaseAdapter := buildDatabaseAdapter(log, mgr.GetClient(), mgr.GetScheme())

	if err := (&controller.WorkspaceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// Node is cluster-scoped but the manager cache above is scoped to
		// workspaceNamespace only; the image pre-stage check (15.4) needs an
		// uncached, direct read instead.
		NodeReader: mgr.GetAPIReader(),
		// The evacuation itself runs inside the workspace Pod, which is the
		// only place holding the working directory and the object store
		// credentials; this side only asks for it and records the result.
		EvacuationRequester: &controller.SupervisorEvacuationRequester{Client: mgr.GetClient()},
		EvacuationConfirmer: controller.StatusEvacuationConfirmer{},
		// Idle detection reads the SSH session count from the workspace
		// instead of the workspace reporting it: the dependency has to run
		// Control Plane -> Workspace Runtime (4.8).
		SSHSessionCounter: &controller.SupervisorSSHSessionCounter{Client: mgr.GetClient()},
		// The Blackboard's changed-file list is pulled from the workspace for
		// the same reason: only the workspace holds the working directory,
		// and the controller stays the single writer of its status (10.3).
		ChangedFilesReporter: &controller.SupervisorChangedFilesReporter{Client: mgr.GetClient()},
		Notify:               controller.HermesNotifier(os.Getenv("HERMES_NOTIFY_URL")),
		// RoutingAdapter/Domain/RoutingExposure make the preview and report
		// entry points reachable through whichever implementation this
		// deployment selected (task 3.1); see buildRoutingAdapter.
		RoutingAdapter:  routingAdapter,
		Domain:          routingDomain,
		RoutingExposure: routingExposure,
		// DatabaseAdapter provisions the branch-dedicated database (task
		// 3.4); see buildDatabaseAdapter.
		DatabaseAdapter: databaseAdapter,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create Workspace controller")
		os.Exit(1)
	}

	maxConcurrentTasks, err := strconv.Atoi(os.Getenv("MAX_CONCURRENT_TASKS"))
	if err != nil || maxConcurrentTasks < 1 {
		log.Error(nil, "MAX_CONCURRENT_TASKS must be a positive integer", "value", os.Getenv("MAX_CONCURRENT_TASKS"))
		os.Exit(1)
	}
	if err := (&controller.TaskQueueReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		MaxConcurrent: maxConcurrentTasks,
		// The quota reading comes from the workspace rather than from a
		// separate call to Anthropic: answering must not itself consume quota
		// (7.10).
		UsageObserver: &controller.SupervisorUsageObserver{Client: mgr.GetClient()},
		// Empty leaves model switching off, which keeps a spent model window
		// behaving like any other stop rather than silently moving work onto a
		// model the operator did not choose.
		ModelFallbacks: splitList(os.Getenv("MODEL_FALLBACKS")),
		Notify:         controller.HermesNotifier(os.Getenv("HERMES_NOTIFY_URL")),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create TaskQueue controller")
		os.Exit(1)
	}

	log.Info("starting manager", "workspaceNamespace", workspaceNamespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// buildRoutingAdapter selects and configures the RoutingAdapter this
// deployment runs (task 3.1, Requirement 3.1/3.2): ROUTING_TYPE picks the
// implementation, everything else (domain, TLS secret, exposure) is plain
// configuration rather than a literal in either implementation.
func buildRoutingAdapter(log logr.Logger, c client.Client, scheme *runtime.Scheme) (routing.RoutingAdapter, string, routing.RoutingExposure) {
	domain := os.Getenv("ROUTING_DOMAIN")
	if domain == "" {
		log.Error(nil, "ROUTING_DOMAIN must be set")
		os.Exit(1)
	}
	tlsSecretName := os.Getenv("ROUTING_TLS_SECRET_NAME")
	if tlsSecretName == "" {
		log.Error(nil, "ROUTING_TLS_SECRET_NAME must be set")
		os.Exit(1)
	}

	var adapter routing.RoutingAdapter
	switch routingType := os.Getenv("ROUTING_TYPE"); routingType {
	case "", "ingress":
		adapter = &routing.IngressAdapter{
			Client:        c,
			Scheme:        scheme,
			Domain:        domain,
			TLSSecretName: tlsSecretName,
			ClassName:     os.Getenv("ROUTING_INGRESS_CLASS_NAME"),
		}
	case "traefik":
		adapter = &routing.TraefikAdapter{
			Client:        c,
			Scheme:        scheme,
			Domain:        domain,
			TLSSecretName: tlsSecretName,
		}
	default:
		log.Error(nil, "ROUTING_TYPE must be \"ingress\" or \"traefik\"", "value", routingType)
		os.Exit(1)
	}

	exposure := routing.RoutingExposure{
		MiddlewareRefs: splitList(os.Getenv("ROUTING_EXPOSURE_MIDDLEWARE_REFS")),
	}
	if raw := os.Getenv("ROUTING_EXPOSURE_ANNOTATIONS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &exposure.Annotations); err != nil {
			log.Error(err, "ROUTING_EXPOSURE_ANNOTATIONS must be a JSON object of string keys/values")
			os.Exit(1)
		}
	}

	return adapter, domain, exposure
}

// buildDatabaseAdapter selects the DatabaseAdapter this deployment runs
// (task 3.4, Requirement 3.5): DATABASE_TYPE picks the implementation,
// mirroring buildRoutingAdapter's ROUTING_TYPE. "none" is how an operator
// without a CNPG-compatible cluster (or who simply does not want a branch
// database) disables the feature; the resulting NoopAdapter is safe as long
// as workspaceTemplate.database.enabled=false also keeps every
// WorkspaceTemplate this chart renders from configuring one (helm chart
// side, values.yaml/templates/workspacetemplate.yaml).
func buildDatabaseAdapter(log logr.Logger, c client.Client, scheme *runtime.Scheme) database.DatabaseAdapter {
	switch databaseType := os.Getenv("DATABASE_TYPE"); databaseType {
	case "", "cnpg":
		return &database.CNPGAdapter{Client: c, Scheme: scheme}
	case "none":
		return &database.NoopAdapter{}
	default:
		log.Error(nil, `DATABASE_TYPE must be "cnpg" or "none"`, "value", databaseType)
		os.Exit(1)
		return nil
	}
}

// splitList reads a comma-separated env var, dropping blanks so an unset or
// empty value yields no entries rather than one empty one.
func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
