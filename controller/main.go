// Command controller runs the Workspace Controller: the single writer of
// Workspace/WorkspaceTemplate-owned substrate inside the workspace-dedicated
// namespace (see internal/controller for the reconciliation logic).
package main

import (
	"os"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
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

	if err := (&controller.WorkspaceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// Node is cluster-scoped but the manager cache above is scoped to
		// workspaceNamespace only; the image pre-stage check (15.4) needs an
		// uncached, direct read instead.
		NodeReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create Workspace controller")
		os.Exit(1)
	}

	log.Info("starting manager", "workspaceNamespace", workspaceNamespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
