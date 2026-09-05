# ワークスペース用ベースイメージ (design.md Platform Manifests / 15.1, 15.2)。
#
# ビルド・push はこのリポジトリの範囲外 (CI 非稼働の Gitea リモートのため手動運用)。
# ビルドコンテキストは apps/devplatform: Session Supervisor を supervisor/ から
# ビルドするため、image/workspace ではコンテキストが足りない。
#   docker build -f apps/devplatform/image/workspace/Dockerfile \
#     -t <registry>/<repo>/workspace:$(date +%Y%m%d)-$(git rev-parse --short HEAD) \
#     apps/devplatform
#   docker push <registry>/<repo>/workspace:<same tag>
#   docker inspect --format '{{index .RepoDigests 0}}' <registry>/<repo>/workspace:<same tag>
# 得られた digest を values.yaml の workspaceImage.digest / workspaceTemplate.image へ設定する。
# タグは日付+短縮コミットSHAの組み合わせのみを使い、latest 等の可変タグは push しない (15.2)。

FROM golang:1.26-alpine AS supervisor-build
WORKDIR /src
COPY supervisor/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/supervisor ./cmd/supervisor

FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# Git: ワークスペースの作業ディレクトリはブランチの作業ツリーそのもの。
# tmux: 端末多重化ツール。開発者と Hermes Agent の双方が同一セッションへ多重接続する (design.md Session Supervisor)。
# curl/ca-certificates/gnupg: Node.js セットアップスクリプトと Playwright の取得に必要。
# Python3/pip: マイグレーションツールを個別に決め打ちしない汎用ランタイムの片方。実際の
# マイグレーションコマンド (node-pg-migrate 等) は対象リポジトリ側のスクリプトに委ねる。
RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        tmux \
        curl \
        ca-certificates \
        gnupg \
        python3 \
        python3-pip \
    && rm -rf /var/lib/apt/lists/*

# Node.js: マイグレーションツールのもう片方の汎用ランタイムであり、Claude Code CLI / Playwright の実行基盤でもある。
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Claude Code CLI。design.md Technology Stack のとおり自動更新を無効化し、
# 払い出されたワークスペース間でバージョンを固定する (5.6)。
ENV DISABLE_AUTOUPDATER=1
RUN npm install -g @anthropic-ai/claude-code

# Playwright: ブラウザ E2E テストの実行環境。依存パッケージ込みで Chromium のみを導入する
# (Firefox/WebKit は対象外。必要になった時点でこのビルド定義に追記する)。
RUN npx --yes playwright install --with-deps chromium

# 共有メモリ不足によるブラウザクラッシュ対策 (design.md 701行付近)。/dev/shm はコンテナ既定で
# 64MB 程度しかなく、Chromium のデフォルト動作 (共有メモリ上にタブ内容を展開) だと容易に枯渇して
# クラッシュする。Playwright/Chromium の起動引数はプロセス起動時にコードで渡す設計のため、
# イメージ単体では強制できない (design.md Implementation Notes: 「本基盤が強制できるのは既定値
# まで」)。そのため既定引数を環境変数として公開するに留め、対象リポジトリの playwright.config
# 側で `args: (process.env.DEVPLATFORM_CHROMIUM_ARGS ?? "").split(" ")` のように読み取ることを
# 規約とする。
ENV DEVPLATFORM_CHROMIUM_ARGS="--disable-dev-shm-usage"

# 配置先パスは controller の resources.go (supervisorBinaryPath) が Pod の
# entrypoint として直接指定するため、両者を一致させる必要がある。
COPY --from=supervisor-build /out/supervisor /usr/local/bin/supervisor

WORKDIR /workspace
