#!/usr/bin/env bash
# release 時に values.yaml の既定イメージ参照を、公開レジストリの不変参照 (digest 付き) に
# 揃える。controller/gateway/workspaceImage の repository/tag/digest 行だけをピンポイントで
# 書き換え、コメントや他の値は一切変更しない。
#
# インストール済みの `yq` は kislyuk/yq (jq ラッパー) で mikefarah/yq ではなく、
# YAML→JSON→YAML の往復でコメントを失ってしまうため使わない。3 箇所固定の書き換えなので
# 素朴な sed で十分。
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: update-image-refs.sh <values.yaml>

Required environment variables:
  CONTROLLER_REPOSITORY CONTROLLER_TAG CONTROLLER_DIGEST
  GATEWAY_REPOSITORY    GATEWAY_TAG    GATEWAY_DIGEST
  WORKSPACE_REPOSITORY  WORKSPACE_TAG  WORKSPACE_DIGEST
EOF
}

if [[ $# -ne 1 ]]; then
  usage >&2
  exit 1
fi
VALUES_FILE="$1"
if [[ ! -f "$VALUES_FILE" ]]; then
  echo "update-image-refs: no such file: $VALUES_FILE" >&2
  exit 1
fi

for var in CONTROLLER_REPOSITORY CONTROLLER_TAG CONTROLLER_DIGEST \
  GATEWAY_REPOSITORY GATEWAY_TAG GATEWAY_DIGEST \
  WORKSPACE_REPOSITORY WORKSPACE_TAG WORKSPACE_DIGEST; do
  : "${!var:?update-image-refs: $var is required}"
done

# sed の置換文字列 (区切り文字 # と、バックリファレンスに解釈される & / \) を無害化する。
sed_escape_repl() {
  printf '%s' "$1" | sed -e 's/[\&#]/\\&/g'
}

# set_field <ブロック見出しの正規表現> <フィールドのインデント> <フィールド名> <値> <見出しから探す行数>
# 見出し行から window 行以内にある「indentフィールド名:」行だけを見つけて値を書き換える。
# コメント行 (# で始まる) は field 名がインデント直後に来ないためマッチしない。
set_field() {
  local header="$1" indent="$2" field="$3" value="$4" window="$5"
  local header_line field_line escaped

  header_line=$(grep -nE "$header" "$VALUES_FILE" | head -1 | cut -d: -f1)
  if [[ -z "$header_line" ]]; then
    echo "update-image-refs: header not found in $VALUES_FILE: $header" >&2
    exit 1
  fi

  field_line=$(awk -v start="$header_line" -v end="$((header_line + window))" \
    -v pat="^${indent}${field}:" \
    'NR>start && NR<=end && $0 ~ pat {print NR; exit}' "$VALUES_FILE")
  if [[ -z "$field_line" ]]; then
    echo "update-image-refs: field not found within $window lines of '$header' in $VALUES_FILE: $field" >&2
    exit 1
  fi

  escaped="$(sed_escape_repl "$value")"
  sed -i "${field_line}s#^\(${indent}${field}: \).*#\1\"${escaped}\"#" "$VALUES_FILE"
}

set_field '^controller:' '    ' repository "$CONTROLLER_REPOSITORY" 6
set_field '^controller:' '    ' tag "$CONTROLLER_TAG" 6
set_field '^controller:' '    ' digest "$CONTROLLER_DIGEST" 6

set_field '^gateway:' '    ' repository "$GATEWAY_REPOSITORY" 6
set_field '^gateway:' '    ' tag "$GATEWAY_TAG" 6
set_field '^gateway:' '    ' digest "$GATEWAY_DIGEST" 6

set_field '^workspaceImage:' '  ' repository "$WORKSPACE_REPOSITORY" 8
set_field '^workspaceImage:' '  ' tag "$WORKSPACE_TAG" 8
set_field '^workspaceImage:' '  ' digest "$WORKSPACE_DIGEST" 8
