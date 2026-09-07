#!/usr/bin/env bash
# chart が提供元の環境に依存せずレンダリングできることを確認する。
# 7.1/7.2 (公開経路への組み込み) の土台として、ローカルでも `helm` だけで実行できる。
set -uo pipefail

CHART_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$CHART_DIR"

MINIMAL_VALUES="hack/testdata/minimal-values.yaml"

fail=0

# 提供元の運用環境にのみ存在する識別子。既定値としても生成物にも現れてはならない
# (要件 2.1, 7.5, 10.3)。ここに追加していく (design.md Portability Gate)。
FORBIDDEN_IDENTIFIERS=(
  "k3s-agent-z440"
  "fickledev.com"
  "tls-fickledev-com"
  "tailscale-operator"
  "59f7eabf-94e5-49d0-85ed-975dfdf27f11"
  "10.42.0.0/16"
  "10.43.0.0/16"
  "argocd-forward-auth-chain"
  "argocd-strip-auth-headers"
  "argocd-local-whitelist"
  "oauth2-proxy-forward-auth-bearer"
  "local-path"
)

check() {
  local name="$1"
  shift
  if "$@"; then
    echo "ok  - $name"
  else
    echo "FAIL - $name"
    fail=1
  fi
}

# 1. 提供元固有の値を一切与えず (利用者が用意したと想定する最小構成のみ)
#    レンダリングが成立し、禁止識別子が生成物に現れない。
minimal_render_no_leak() {
  local out
  out="$(helm template devplatform . -f "$MINIMAL_VALUES" 2>&1)" || return 1
  for id in "${FORBIDDEN_IDENTIFIERS[@]}"; do
    if grep -qF -- "$id" <<<"$out"; then
      echo "  forbidden identifier leaked: $id" >&2
      return 1
    fi
  done
  return 0
}
check "minimal render succeeds and leaks no forbidden identifier" minimal_render_no_leak

# 2. 任意依存をすべて無効にした構成でもレンダリングが成立する。
optional_deps_disabled() {
  helm template devplatform . -f "$MINIMAL_VALUES" \
    --set evacuationCredentials.autoProvision.enabled=false \
    --set infisical.sshCa.enabled=false \
    --set infisical.evacuation.enabled=false \
    --set controller.persistence.enabled=false \
    >/dev/null 2>&1
}
check "render succeeds with all optional dependencies disabled" optional_deps_disabled

# 3. 必須値を欠くとレンダリングが失敗し、不足している値の名前がエラーに含まれる。
required_value_missing_fails() {
  local out
  out="$(helm template devplatform . -f "$MINIMAL_VALUES" --set taskQueue.maxConcurrent=null 2>&1)" && return 1
  grep -q "maxConcurrent" <<<"$out" || return 1

  out="$(helm template devplatform . --set gateway.domain=example.internal --set gateway.tls.secretName=null 2>&1)" && return 1
  grep -q "secretName" <<<"$out" || return 1

  # values.schema.json による必須値検証 (2.3)。
  out="$(helm template devplatform . -f "$MINIMAL_VALUES" --set workspaceTemplate.database.clusterRef= 2>&1)" && return 1
  grep -q "clusterRef" <<<"$out" || return 1

  out="$(helm template devplatform . -f "$MINIMAL_VALUES" \
    --set infisical.enabled=true --set infisical.projectId= 2>&1)" && return 1
  grep -q "projectId" <<<"$out"
}
check "rendering fails and names the missing required value" required_value_missing_fails

# 4. 有効にした機能に必要な依存が指定されていない場合、レンダリングが失敗する
#    (条件付き必須, 3.7/4.10)。ここでは退避先S3資格情報の自動発行を例にする。
conditional_dependency_missing_fails() {
  local out
  out="$(helm template devplatform . -f "$MINIMAL_VALUES" \
    --set evacuationCredentials.autoProvision.enabled=true \
    --set evacuationCredentials.autoProvision.garage.namespace= 2>&1)" && return 1
  grep -q "namespace" <<<"$out" || return 1

  # Infisical Operator による同期を有効にしたが、接続先プロジェクトの情報がない場合 (3.3)。
  out="$(helm template devplatform . -f "$MINIMAL_VALUES" \
    --set infisical.enabled=true 2>&1)" && return 1
  grep -q "projectId" <<<"$out"
}
check "enabling a feature without its required dependency fails rendering" conditional_dependency_missing_fails

exit $fail
