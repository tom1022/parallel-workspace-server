#!/usr/bin/env bash
# hack/update-image-refs.sh が対象 3 ブロック (controller/gateway/workspaceImage の
# repository/tag/digest) だけを書き換え、コメントや他の値を変更しないことを確認する。
# bats は未導入のため、hack/portability-check.test.sh と同じ assert ベースの
# 最小ランナーを自作する。
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
SCRIPT="$SCRIPT_DIR/update-image-refs.sh"

tests_run=0
tests_failed=0

# assert <説明> <pass|fail> <関数> [引数...]
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

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

target="$work/values.yaml"
cp "$CHART_DIR/values.yaml" "$target"

run_with_env() {
  CONTROLLER_REPOSITORY="ghcr.io/test/devplatform-controller" \
    CONTROLLER_TAG="v1.2.3" \
    CONTROLLER_DIGEST="sha256:1111111111111111111111111111111111111111111111111111111111111111" \
    GATEWAY_REPOSITORY="ghcr.io/test/devplatform-gateway" \
    GATEWAY_TAG="v1.2.3" \
    GATEWAY_DIGEST="sha256:2222222222222222222222222222222222222222222222222222222222222222" \
    WORKSPACE_REPOSITORY="ghcr.io/test/devplatform-workspace" \
    WORKSPACE_TAG="v1.2.3" \
    WORKSPACE_DIGEST="sha256:3333333333333333333333333333333333333333333333333333333333333333" \
    "$SCRIPT" "$1"
}

# --- 正常系: 3 ブロック (9 行) だけが書き換わる ---
run_with_env "$target"

only_nine_lines_changed() {
  local n
  n=$(diff "$CHART_DIR/values.yaml" "$target" | grep -c '^> ')
  [[ "$n" -eq 9 ]]
}
assert "changed lines == 9 (3 fields x 3 images)" pass only_nine_lines_changed

controller_updated() {
  grep -q 'repository: "ghcr.io/test/devplatform-controller"' "$target" &&
    grep -q 'tag: "v1.2.3"' "$target" &&
    grep -q 'digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"' "$target"
}
assert "controller block updated" pass controller_updated

gateway_updated() {
  grep -q 'repository: "ghcr.io/test/devplatform-gateway"' "$target" &&
    grep -q 'digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"' "$target"
}
assert "gateway block updated" pass gateway_updated

workspace_updated() {
  grep -q 'repository: "ghcr.io/test/devplatform-workspace"' "$target" &&
    grep -q 'digest: "sha256:3333333333333333333333333333333333333333333333333333333333333333"' "$target"
}
assert "workspaceImage block updated" pass workspace_updated

comments_preserved() {
  grep -qF '# 例: "sha256:0000000000000000000000000000000000000000000000000000000000000"' "$target" &&
    grep -qF '# ワークスペース用ベースイメージ' "$target"
}
assert "comments untouched" pass comments_preserved

# --- 異常系: 必須環境変数が欠けていれば失敗する ---
missing_env_fails() {
  local tmp="$work/values-missing-env.yaml"
  cp "$CHART_DIR/values.yaml" "$tmp"
  env -i PATH="$PATH" "$SCRIPT" "$tmp"
}
assert "missing required env var fails" fail missing_env_fails

# --- 異常系: 対象ファイルが存在しなければ失敗する ---
missing_file_fails() {
  run_with_env "$work/does-not-exist.yaml"
}
assert "missing target file fails" fail missing_file_fails

# --- 値に sed の特殊文字 (&) が含まれても壊れない ---
special_chars_safe() {
  local tmp="$work/values-special.yaml"
  cp "$CHART_DIR/values.yaml" "$tmp"
  CONTROLLER_REPOSITORY='ghcr.io/test/a&b' \
    CONTROLLER_TAG='v1.0.0' \
    CONTROLLER_DIGEST='sha256:aaaa' \
    GATEWAY_REPOSITORY='ghcr.io/test/gateway' \
    GATEWAY_TAG='v1.0.0' \
    GATEWAY_DIGEST='sha256:bbbb' \
    WORKSPACE_REPOSITORY='ghcr.io/test/workspace' \
    WORKSPACE_TAG='v1.0.0' \
    WORKSPACE_DIGEST='sha256:cccc' \
    "$SCRIPT" "$tmp" || return 1
  grep -qF 'repository: "ghcr.io/test/a&b"' "$tmp"
}
assert "value containing & is preserved literally" pass special_chars_safe

echo
echo "ran $tests_run, failed $tests_failed"
[[ "$tests_failed" -eq 0 ]]
