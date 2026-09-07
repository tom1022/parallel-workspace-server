// This file covers task 8.2: rendering the Blackboard into each workspace's
// CLAUDE.md (design.md "Blackboard Reconciler" State Management, Requirement
// 10.4/10.5/10.6/10.9).
//
// The document is delivered as a ConfigMap rather than written into the
// workspace: the control plane never reaches into a working directory, and the
// mount lets the Session Supervisor pick the moment the new revision takes
// effect (10.6). Each regeneration is a single write of a single value, which
// is what makes a half-applied document unobservable without a lock (10.9).
package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// ClaudeMDKey is the ConfigMap key, and the file name it surfaces as in
	// the mount.
	ClaudeMDKey = "CLAUDE.md"

	// ClaudeMDBranchMarker separates the part every workspace shares from the
	// part that names this branch. 10.5 fixes that order — shared prefix
	// first — because Claude Code's prompt cache is only reused across
	// workspaces while the prefix matches byte for byte.
	ClaudeMDBranchMarker = "\n<!-- workspace -->\n"

	// claudeMDPreamble opens every document. It is a constant rather than
	// anything derived, for the same prefix reason as the marker above.
	claudeMDPreamble = `# 並行開発プラットフォーム

このリポジトリは複数のブランチで同時に作業が進んでいる。以下は現在アクティブな
全ブランチの作業状況で、内容は全ワークスペースで同一。

- 他ブランチが変更中のファイルには手を入れない
- 他ブランチの公開インターフェースに依存する場合は記載された名前に合わせる
`
)

// ClaudeMDConfigMapName is the document's ConfigMap for the workspace whose
// owned resources are named resourceName.
func ClaudeMDConfigMapName(resourceName string) string {
	return resourceName + "-claude-md"
}

// RenderClaudeMD builds the document handed to the workspace on branch. The
// entries are rendered whole, in the order ActiveBlackboardEntries fixed, so
// every workspace's shared prefix is identical (10.5).
func RenderClaudeMD(entries []devplatformv1alpha1.BlackboardEntry, branch string) string {
	var b strings.Builder
	b.WriteString(claudeMDPreamble)
	for _, e := range entries {
		fmt.Fprintf(&b, "\n## %s\n\n", e.Branch)
		fmt.Fprintf(&b, "- 作業概要: %s\n", orNone(e.Summary))
		fmt.Fprintf(&b, "- 公開インターフェース: %s\n", orNone(strings.Join(e.PublicInterfaces, ", ")))
		fmt.Fprintf(&b, "- 変更中ファイル: %s\n", orNone(strings.Join(e.ChangedFiles, ", ")))
	}
	b.WriteString(ClaudeMDBranchMarker)
	fmt.Fprintf(&b, "\nこのワークスペースが担当するブランチは `%s`。上記のうち自ブランチの項目は自分の作業。\n", branch)
	return b.String()
}

func orNone(v string) string {
	if v == "" {
		return "なし"
	}
	return v
}

// reconcileClaudeMD regenerates the workspace's document from the branches
// currently active in its namespace and stores it, writing only when the
// content actually changed so a steady state produces no ConfigMap churn.
//
// ponytail: each workspace renders its own document on its own reconcile pass
// rather than one pass rewriting everybody's, so another branch's update
// reaches this one within the Ready requeue interval instead of immediately.
// Fan the write out if that lag ever matters.
func (r *WorkspaceReconciler) reconcileClaudeMD(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName string) error {
	entries, err := ActiveBlackboardEntries(ctx, r.Client, ws.Namespace)
	if err != nil {
		return err
	}
	document := RenderClaudeMD(entries, ws.Spec.Branch)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClaudeMDConfigMapName(resourceName),
			Namespace: ws.Namespace,
			Labels:    workspaceLabels(ws),
		},
	}
	if err := controllerutil.SetControllerReference(ws, cm, r.Scheme); err != nil {
		return err
	}

	var current corev1.ConfigMap
	err = r.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, &current)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		cm.Data = map[string]string{ClaudeMDKey: document}
		return r.ensureCreated(ctx, cm)
	}
	if current.Data[ClaudeMDKey] == document {
		return nil
	}
	// The whole document goes over in one write; there is no revision in which
	// a mount holds part of an old one and part of a new one (10.9).
	current.Data = map[string]string{ClaudeMDKey: document}
	return r.Update(ctx, &current)
}
