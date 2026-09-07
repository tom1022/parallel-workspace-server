# devplatform

自律的に開発タスクを並行実行するための基盤。エージェント (Claude Code) がブランチ
ごとに隔離された Kubernetes ワークスペース上で実装・テストを自律的に進め、利用者は
共有端末ゲートウェイ経由でブラウザや SSH からその進行に立ち会い、必要なら操作を
引き継げる。

- **Workspace Controller** (`controller/`) — ワークスペース (Workspace CR) の払い出し
  からタスクキューイング・アイドル検知・退避・破棄までを、ワークスペース専用
  ネームスペース内に閉じて管理する Kubernetes controller。
- **Terminal Gateway** (`gateway/`) — ブラウザ・SSH からワークスペースへの経路を提供し、
  SSH 証明書の発行と OIDC 認可を担う共有端末。
- **Supervisor** (`supervisor/`) — 各ワークスペース Pod 内で Claude Code セッションを
  tmux 上に保持し、ヘルスチェック・退避・DB ブートストラップ・テストの自己修復ループを
  行う。

## 想定する利用場面

複数の開発タスクを並行して隔離されたワークスペースで進めたい運用者が、自分の
Kubernetes クラスタへこの基盤を配備し、Web ターミナル (または SSH) 経由でエージェント
駆動の開発を行い、必要に応じて進行を確認・介入する、という使い方を想定している。

この chart 自体は制御プレーンとゲートウェイだけを配備する。ワークスペース実体
(Workspace CR・StatefulSet・PVC・ブランチ専用データベース) はこの chart には含まれず、
制御プレーンが実行時に払い出す。

## 前提

- Kubernetes v1.31 以上 (`Chart.yaml` の `kubeVersion` で強制される)。
- Helm 3 以上。CRD は `crds/` に置いているため `helm install`/`upgrade` で自動的に
  適用される (更新については後述)。

配備前に、同じクラスタで以下が動いている必要がある。

| 依存 | 用途 | 必須/任意 |
|---|---|---|
| Ingress 用ルーティング実装 (現状 Traefik `IngressRoute` のみ対応) | ゲートウェイへの外部到達経路 | 必須 |
| 証明書発行の仕組み (cert-manager 等) | `gateway.tls.secretName` の TLS 証明書 | 必須 |
| OIDC 対応の認可サーバ | ゲートウェイの認証 (`gateway.oidc.*`) | 必須 |
| Infisical Kubernetes Operator | 秘匿情報の同期 (`infisical.*`) | 必須 |
| ブランチ専用データベース (PostgreSQL 系。CloudNativePG 等) | ワークスペースごとの作業用 DB | 必須 (この chart には含まれない。利用者側で用意し `workspaceTemplate.database.clusterRef` で参照する) |
| S3 互換オブジェクトストレージ (Garage 等) | 作業内容の退避先 | 必須。既定 (`evacuationCredentials.autoProvision.enabled: true`) はクラスタ内 Garage 前提で資格情報を自動発行する。外部 S3 を使う場合は無効化して自分で用意する |
| Tailscale operator 等の VPN | 統合開発環境からの任意経路 | 任意。`subnetRouterNamespace` を設定した場合のみ経路が開く |

将来のリリースでは、ルーティング実装の差し替え・秘匿情報同期の必須撤廃・
基盤自身によるローカル認証など、上表の一部を任意化する予定である。現時点の
必須/任意は `values.schema.json` が配備前に検証する。

## 配備手順

1. 上表の依存を配備する。
2. Infisical のプロジェクトに、下表の利用者提供キーを登録する (値をリポジトリへ
   置いてはならない)。
3. `values.yaml` を配備先に合わせて調整する (次節参照)。`helm template` で
   レンダリング結果を確認する。
4. `helm install devplatform . -f <自分の values ファイル>` で配備する。
5. `devplatform-gateway` Pod のログに SSH 認証局・OIDC 検証まわりのエラーが
   出ていないこと、`workspaceNamespace` に `devplatform-workspace-ssh-ca` と
   退避用 Secret (`workspaceTemplate.evacuation.secretRef`) が生成されている
   ことを確認する。
6. `<gateway.apiHost>.<gateway.domain>` の `/healthz` エンドポイントへの到達で
   ゲートウェイの起動を確認できる。

### つまずきやすい前提

- `gateway.domain` 直下のワイルドカード (または個別) 証明書と、`gateway.tls.secretName`
  の Secret がゲートウェイと同じネームスペースに用意されている必要がある。準備が
  先に終わっていないとゲートウェイの受け口自体が張られない。
- `gateway.oidc.issuer`/`audience`/`jwksUrl` は認可サーバ側のクライアント登録と
  一致していないと、正当なトークンでも一律に拒否される。
- `workspaceTemplate.database.clusterRef` は Workspace 払い出し時にだけ参照される。
  この chart 自体はデータベースを配備しないため、先に用意しておく。
- `evacuationCredentials.autoProvision.enabled: true` (既定) のまま外部 S3 を使うと、
  存在しない Garage Pod への到達を試みて Job が失敗し続ける。外部 S3 を使う場合は
  必ず無効化する。
- CRD (`crds/`) は Helm の `upgrade` では更新されない。スキーマを変える版を導入する
  ときは `kubectl apply -f crds/` を先に実行する。

## values 一覧

`values.schema.json` が「必須」列を配備前に検証する。「省略時」は値を省略した場合の
既定の挙動であり、必ずしも安全な値ではない (特に秘匿情報同期系は既定で有効)。

| 値 | 意味 | 既定 | 必須 |
|---|---|---|---|
| `controller.image.repository`/`tag`/`digest` | Workspace Controller イメージ参照 | 公開レジストリの既定イメージ | 省略可 |
| `controller.nodeSelector` | 制御プレーンの配置制約 | `{}` (制約なし) | 省略可 |
| `controller.persistence.enabled`/`storageClass`/`size` | 制御プレーンのローカルキャッシュ用 PVC (現状未使用) | 無効、StorageClass はクラスタ既定 | 省略可 |
| `gateway.image.repository`/`tag`/`digest` | Terminal Gateway イメージ参照 | 公開レジストリの既定イメージ | 省略可 |
| `gateway.replicas`/`port` | ゲートウェイの Pod 数・待受ポート | `1` / `8080` | 省略可 |
| `gateway.domain` | ワークスペースのホスト名が属する公開ドメイン | なし | **必須** |
| `gateway.apiHost` | `/api/` の受け口ホスト名 (単一ラベル) | `devworkspaces` | 省略可 |
| `gateway.tls.secretName` | ドメインへの TLS 証明書を保持する Secret 名 | なし | **必須** |
| `gateway.oidc.issuer`/`audience`/`jwksUrl` | 外部 IdP の Bearer JWT 検証パラメータ | なし | **必須** |
| `gateway.ssh.*` | SSH 認証局の Secret 名・証明書 TTL 等 | チャート既定のリソース名 | 省略可 |
| `gateway.nodeSelector` | ゲートウェイの配置制約 | `{}` (制約なし) | 省略可 |
| `routing.workspaceMiddlewares`/`apiMiddlewares` | Traefik IngressRoute に付与する中間処理の完全修飾名 | `[]` (付与しない) | 省略可 |
| `routing.ingressNamespace`/`ingressPodSelector` | ルーティング実装がワークスペースへ到達するための NetworkPolicy 許可元 | `kube-system` / `app.kubernetes.io/name: traefik` (Traefik 同梱の k3s を既定と仮定) | 省略可。空にするとこの経路を許可しない |
| `workspaceNamespace` | Workspace/WorkspaceTemplate を置くネームスペース | `devplatform-workspaces` | **必須** |
| `subnetRouterNamespace` | VPN 等、任意経路の subnet router が居るネームスペース | `""` (経路を開かない) | 省略可 |
| `database.namespace` | ブランチ専用データベースの egress 許可先ネームスペース | `devplatform-db` | 省略可。空にすると egress 規則を生成しない |
| `objectStorage.namespace` | 退避先オブジェクトストレージの egress 許可先ネームスペース | `garage` | 省略可。空にすると egress 規則を生成しない |
| `network.excludeCIDRs` | 外部への egress 許可から除くクラスタ内アドレス範囲 (Pod/Service CIDR 等) | `[]` (除外しない) | 省略可 |
| `workspaceTemplate.resources`/`storage` | 既定 WorkspaceTemplate の CPU/メモリ/作業ディレクトリ容量 | チャート既定値 | 省略可 |
| `workspaceTemplate.nodeName` | ワークスペースの配置先ノード | `""` (制約なし) | 省略可。ノード固定の StorageClass を使うなら実質必須 |
| `workspaceTemplate.priorityClassName` | ワークスペース Pod の PriorityClass | `devplatform-workspace` (この chart が生成) | 省略可 |
| `workspaceTemplate.model` | 既定モデル | `""` (Claude Code 既定) | 省略可 |
| `workspaceTemplate.database.clusterRef` | ブランチ専用データベースへの参照名 | なし | **必須** |
| `workspaceTemplate.auth.secretRef` | Claude Code 長期認証情報を保持する Secret 名 | なし | **必須** |
| `workspaceTemplate.evacuation.bucket`/`secretRef` | 退避先バケット名・S3 資格情報 Secret 名 | なし | **必須** |
| `taskQueue.maxConcurrent` | 同時実行 Claude Code タスク数の上限 | `2` | **必須** |
| `taskQueue.modelFallbacks` | モデル別利用枠が尽きたときの切り替え順 | `[]` (切り替えない) | 省略可 |
| `workspaceQuota.*` | ワークスペース用ネームスペースの ResourceQuota | クラスタ数個分の保守的な値 | 省略可 |
| `evacuationCredentials.autoProvision.enabled` | クラスタ内 Garage への退避資格情報自動発行 | `true` | 省略可 |
| `evacuationCredentials.autoProvision.garage.*` | 自動発行が参照する Garage の namespace/Pod セレクタ/鍵名 | チャート既定値 | `autoProvision.enabled: true` の場合のみ必須 |
| `infisical.projectId`/`environmentSlug`/`secretPath` | 秘匿情報同期元の Infisical プロジェクト | プロジェクト ID はなし | **必須** |
| `infisical.authRef.name`/`namespace` | Infisical Operator の機械 ID (`InfisicalMachineIdentity`) 参照 | なし | **必須** |
| `infisical.sshCa.enabled`/`evacuation.enabled` | SSH 認証局・退避資格情報を Infisical から同期するか | `false` (クラスタ内発行を使う) | 省略可 |
| `workspaceImage.repository`/`tag`/`digest` | ワークスペースベースイメージ参照 | 公開レジストリの既定イメージ | 省略可 |

## 利用者が用意する秘匿情報

外部のアカウント・契約がないと発行できないものだけを、`infisical.*` で指す
Infisical プロジェクトに登録する。値をリポジトリへ置いてはならない。

| Infisical のキー | 中身 | 必須 |
|---|---|---|
| `DEVPLATFORM_WORKSPACE_CLAUDE_CODE_CREDENTIALS` | Claude Code の長期認証情報 (`credentials.json` の内容そのまま) | 必須 |
| `DEVPLATFORM_INFERENCE_API_KEY` | 推論バックエンドの API キー (Claude Code のサブスクリプション認証とは別系統) | 必須 |
| `DEVPLATFORM_SSH_CA_PRIVATE_KEY` | 自前の SSH 認証局を持ち込む場合の秘密鍵 | `infisical.sshCa.enabled: true` の場合のみ |
| `DEVPLATFORM_EVACUATION_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY` | 外部 S3 を退避先にする場合の資格情報 | `infisical.evacuation.enabled: true` の場合のみ |

## 自動で用意されるもの

以下は外部に発行元が存在しないため、配備時にクラスタ内で発行される。手作業は不要。
いずれも**既にあれば何もしない**ため、同期を繰り返しても値は変わらない。

| 資格情報 | 発行する主体 | 置き場所 |
|---|---|---|
| SSH 認証局の秘密鍵 | Terminal Gateway が起動時に生成 | リリースのネームスペース (`gateway.ssh.caSecretName`) |
| SSH 認証局の公開鍵 | Terminal Gateway が秘密鍵から導出 | `workspaceNamespace` (`gateway.ssh.caPublicSecretName`) |
| 退避先 S3 の資格情報 | `devplatform-evacuation-credentials` Job が Garage の鍵を発行 | `workspaceNamespace` (`workspaceTemplate.evacuation.secretRef`) |
| データベースの資格情報 | 利用するデータベースオペレータが bootstrap 時に生成 | 利用者が用意したデータベースの流儀に従う |

これらの Secret は chart がレンダリングしない。GitOps 側で `helm template` により
マニフェストを生成する運用では Helm の `lookup` がクラスタを参照できず、
「既にあれば維持する」をテンプレートで表現できないためである。テンプレートに
置けば同期のたびに鍵が置き換わり、発行済みの SSH 証明書が失効し、退避データへ
到達できなくなる。生成された Secret には `argocd.argoproj.io/sync-options:
Prune=false,Delete=false` を付け、一律の prune/selfHeal から守っている。

再発行が必要になった場合 (鍵の更新・漏洩時) は、対象の Secret を削除して
ゲートウェイ Pod を再起動するか、Job を再実行する。SSH 認証局を入れ替えると
発行済みの証明書はすべて無効になる。退避先の鍵は Garage 側に同名
(`evacuationCredentials.autoProvision.garage.keyName`) の鍵が残っている限り
同じ値が復元されるため、Secret を消しても退避データへの到達性は失われない。

## 自分の資格情報を使う

自動発行された値ではなく自前の値を使いたい場合、いずれも**その Secret を先に
用意する**ことが上書き経路になる。自動発行側は既存の Secret を尊重して何もしない。

- **SSH 認証局**: `gateway.ssh.caSecretName` の Secret を `gateway.ssh.caKeySecretKey`
  キー (OpenSSH 形式の秘密鍵) 付きで用意する。Infisical から同期するなら
  `infisical.sshCa.enabled: true` にして `DEVPLATFORM_SSH_CA_PRIVATE_KEY` を登録する。
  公開鍵はゲートウェイが秘密鍵から導出するため登録不要。
- **退避先 S3**: `evacuationCredentials.autoProvision.enabled: false` にし、
  `workspaceTemplate.evacuation.secretRef` の Secret を `access-key` / `secret-key`
  の 2 キーで用意する。Infisical から同期するなら `infisical.evacuation.enabled: true`
  にして `DEVPLATFORM_EVACUATION_ACCESS_KEY_ID` と
  `DEVPLATFORM_EVACUATION_SECRET_ACCESS_KEY` を登録する。外部 S3 を退避先にする場合は
  併せて `workspaceTemplate.evacuation.bucket` と、S3 エンドポイントの指定
  (`controller/internal/controller/resources.go` 参照。現状は差し替え可能になっていない) を
  配備先に合わせる。
- **データベース**: 利用するデータベースオペレータ側の設定で、生成される
  superuser Secret を指定の Secret に差し替える。

## 動作確認 (portability check)

`hack/portability-check.sh` は、提供元の環境固有値を与えない状態でのレンダリング
成立・任意依存を無効にした構成での成立・必須値/条件付き依存の欠落時の失敗・
禁止識別子 (ノード名・ドメイン・IP アドレス・外部発行済み ID 等) の非混入を
`helm template` だけで確認する。chart を変更した際はローカルで実行できる。

```sh
./hack/portability-check.sh
```

## イメージ

`controller/`, `gateway/`, `image/workspace/` の Dockerfile が生成する 3 つのイメージを
使う。本リポジトリのリモートでは CI が稼働していないため、ビルドと push は手動運用とし、
タグは日付 + 短縮コミット SHA のような一意値のみを使う (`latest` を使わない)。
イメージは環境中立で、認証局・ホスト名・利用者固有の値を焼き込まない。
