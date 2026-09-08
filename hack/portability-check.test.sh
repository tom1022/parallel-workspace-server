#!/usr/bin/env bash
# hack/portability-check.sh の各検査関数が「意図的な混入」を検知できることを確認する。
# bats は未導入のため、assert ベースの最小ランナーを自作する。フレームワークは導入しない。
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# portability-check.sh は「直接実行された場合のみ」検査を実行して exit するようガードされている
# ため、source しても各関数 (minimal_render_no_leak 等) と変数だけを取り込める。
# shellcheck disable=SC1091
source "$SCRIPT_DIR/portability-check.sh"

tests_run=0
tests_failed=0

# assert <説明> <pass|fail> <関数> [引数...]
# 「pass」は関数が成功する(真を返す)ことを、「fail」は失敗する(偽を返す)ことを期待する。
assert() {
  local desc="$1" expect="$2"
  shift 2
  tests_run=$((tests_run + 1))
  local actual
  if "$@" >/dev/null 2>&1; then actual=pass; else actual=fail; fi
  if [[ "$actual" == "$expect" ]]; then
    echo "ok  - $desc"
  else
    echo "FAIL - $desc (expected $expect, got $actual)"
    tests_failed=$((tests_failed + 1))
  fi
}

# --- ベースライン: 意図的な混入がなければ全検査が通ること ---
assert "baseline: minimal render leaks no forbidden identifier" pass minimal_render_no_leak
assert "baseline: no secret-like string in worktree" pass no_secrets_in_worktree

# --- FORBIDDEN_IDENTIFIERS を混入させると検知すること ---
overlay="$(mktemp "$CHART_DIR/hack/testdata/tmp-forbidden-XXXXXX.yaml")"
cat >"$overlay" <<EOF
gateway:
  domain: "${FORBIDDEN_IDENTIFIERS[0]}"
EOF
assert "injected forbidden identifier is detected" fail minimal_render_no_leak -f "$overlay"
rm -f "$overlay"

# --- 秘匿情報らしき文字列を混入させると検知すること ---
secret_file="$(mktemp "$CHART_DIR/hack/testdata/tmp-secret-XXXXXX.yaml")"
cat >"$secret_file" <<'EOF'
extraConfig:
  password: "N0tAPlaceholderReallySecretValue"
EOF
assert "injected password value is detected" fail no_secrets_in_worktree
rm -f "$secret_file"

key_file="$(mktemp "$CHART_DIR/hack/testdata/tmp-awskey-XXXXXX.env")"
echo 'AWS_ACCESS_KEY_ID=AKIA1234567890ABCDEF' >"$key_file"
assert "injected AWS access key is detected" fail no_secrets_in_worktree
rm -f "$key_file"

# --- 既知のプレースホルダ/テンプレート式は誤検知しないこと ---
placeholder_file="$(mktemp "$CHART_DIR/hack/testdata/tmp-placeholder-XXXXXX.yaml")"
cat >"$placeholder_file" <<'EOF'
extraConfig:
  secretRef: "{{ .Values.workspaceTemplate.evacuation.secretRef }}"
  token: "changeme"
EOF
assert "placeholder/template-expression values are not flagged" pass no_secrets_in_worktree
rm -f "$placeholder_file"

# GitHub Actions の式構文 (${{ secrets.XXX }}) はべた書きの秘匿値ではない
# (.github/workflows/release.yml:27 相当の再現ケース)。
workflow_expr_file="$(mktemp "$CHART_DIR/hack/testdata/tmp-workflow-XXXXXX.yaml")"
cat >"$workflow_expr_file" <<'EOF'
steps:
  - uses: docker/login-action@v3
    with:
      password: ${{ secrets.GITHUB_TOKEN }}
EOF
assert "GitHub Actions secrets expression is not flagged" pass no_secrets_in_worktree
rm -f "$workflow_expr_file"

echo
echo "$tests_run tests, $tests_failed failed"
exit $((tests_failed > 0))
