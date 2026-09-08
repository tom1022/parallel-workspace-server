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
  out="$(helm template devplatform . -f "$MINIMAL_VALUES" "$@" 2>&1)" || return 1
  for id in "${FORBIDDEN_IDENTIFIERS[@]}"; do
    if grep -qF -- "$id" <<<"$out"; then
      echo "  forbidden identifier leaked: $id" >&2
      return 1
    fi
  done
  return 0
}
# 2. 任意依存をすべて無効にした構成でもレンダリングが成立する。
optional_deps_disabled() {
  helm template devplatform . -f "$MINIMAL_VALUES" \
    --set evacuationCredentials.autoProvision.enabled=false \
    --set infisical.sshCa.enabled=false \
    --set infisical.evacuation.enabled=false \
    --set controller.persistence.enabled=false \
    --set workspaceTemplate.database.enabled=false \
    --set createWorkspaceNamespace=false \
    --set workspaceQuota.enabled=false \
    --set workspaceNetworkPolicy.enabled=false \
    --set priorityClass.enabled=false \
    --set controller.clusterNodeAccess.enabled=false \
    >/dev/null 2>&1
}
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
# 5. 外部の識別基盤(OIDC)連携は任意機能 (4.2)。無効(既定)のままなら issuer/audience/jwksUrl
#    を与えなくてもレンダリングが成立し、有効にした場合だけそれらが必須になる (条件付き必須, 4.8-4.10)。
oidc_conditionally_required() {
  local out
  helm template devplatform . \
    --set gateway.domain=example.internal --set gateway.tls.secretName=example-gateway-tls \
    >/dev/null 2>&1 || return 1

  out="$(helm template devplatform . \
    --set gateway.domain=example.internal --set gateway.tls.secretName=example-gateway-tls \
    --set gateway.oidc.enabled=true 2>&1)" && return 1
  grep -q "issuer" <<<"$out"
}
# 6. ワーキングツリーに秘匿情報らしき文字列が紛れ込んでいない (git-secrets 相当の簡易検査)。
# PEM 秘密鍵ヘッダと AWS アクセスキー ID は語として一意なため repo 全体を対象にできるが、
# password/secret/token 等はソース中の識別子 (変数名・struct フィールド名) と大量に衝突するため、
# 対象を設定ファイル (yaml/yml/json/env) に絞る。
AWS_KEY_ALLOWLIST="AKIAIOSFODNN7EXAMPLE" # AWS 公式ドキュメントの例示アクセスキー (実キーではない)。
# この検査自体のテスト用フィクスチャ (portability-check.test.sh) は、検知確認のため
# 意図的に秘匿情報らしき文字列を埋め込んでいるので対象から除く。
SELF_TEST_EXCLUDE="portability-check.test.sh"
no_secrets_in_worktree() {
  local hits
  hits="$(grep -rInE --exclude-dir=.git --exclude="$SELF_TEST_EXCLUDE" \
    -e '-----BEGIN (RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----' \
    -e 'AKIA[0-9A-Z]{16}' \
    "$CHART_DIR" 2>/dev/null | grep -v "$AWS_KEY_ALLOWLIST")"
  if [[ -n "$hits" ]]; then
    echo "  secret-like pattern found:" >&2
    echo "$hits" >&2
    return 1
  fi

  local candidate value
  while IFS= read -r candidate; do
    [[ -z "$candidate" ]] && continue
    value="${candidate##*[:=]}"
    value="$(tr -d '[:space:]"'"'"'' <<<"$value")"
    # Helm テンプレート式や、明らかなプレースホルダは実際の秘匿情報ではないため除外する。
    case "$value" in
      '' | '{{'* | [Cc]hange[Mm]e* | [Ee]xample* | [Xx][Xx][Xx]* | [Tt][Oo][Dd][Oo] | \
      [Nn]ull | [Nn]one | [Rr]edacted* | [Dd]ummy* | [Rr]eplace*)
        continue
        ;;
    esac
    echo "  possible hardcoded credential: $candidate" >&2
    return 1
  # 注意: GNU grep は --include/--exclude を指定順で評価するため、--exclude を
  # --include より先に書くと --include によるファイル種別の絞り込みが効かなくなる。
  done < <(grep -rInE \
    --include='*.yaml' --include='*.yml' --include='*.json' --include='*.env' \
    --exclude-dir=.git --exclude="$SELF_TEST_EXCLUDE" \
    -i '(password|passwd|secret|token|apikey|api_key)[[:space:]]*[:=][[:space:]]*[^[:space:]]+' \
    "$CHART_DIR" 2>/dev/null)
  return 0
}
# テストランナー (portability-check.test.sh) が関数だけを再利用できるよう、
# source された場合はここから下の検査実行 (helm 呼び出しを伴う) と exit を走らせない。
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  check "minimal render succeeds and leaks no forbidden identifier" minimal_render_no_leak
  check "render succeeds with all optional dependencies disabled" optional_deps_disabled
  check "rendering fails and names the missing required value" required_value_missing_fails
  check "enabling a feature without its required dependency fails rendering" conditional_dependency_missing_fails
  check "oidc issuer/audience/jwksUrl are required only when gateway.oidc.enabled=true" oidc_conditionally_required
  check "no secret-like strings in worktree" no_secrets_in_worktree
  exit $fail
fi
