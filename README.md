# devplatform

自律並行開発基盤の制御プレーン (Workspace Controller) と共有端末ゲートウェイ
(Terminal Gateway) を配備する Helm chart。ワークスペース実体 (Workspace CR・
StatefulSet・PVC・ブランチ専用データベース) はこの chart には含まれず、
制御プレーンが実行時に払い出す。

## 前提となるクラスタ構成

配備前に以下が同じクラスタで動いている必要がある。

| 依存 | 用途 | 本リポジトリ内の定義 |
|---|---|---|
| Argo CD + ApplicationSet | `apps/*` の同期 | `apps/argocd/` |
| Infisical Kubernetes Operator | 利用者が用意する秘匿情報の同期 | `apps/infisical-operator/` |
| CloudNativePG operator | ブランチ専用データベース | `apps/cnpg-operator/` |
| Garage (S3 互換) | 作業内容の退避先 | `apps/garage/` |
| cert-manager + reflector | ワークスペースのホスト名を含むワイルドカード証明書 | `apps/cluster-issuer/` |
| OIDC 認可サーバ + oauth2-proxy | ゲートウェイの認証 | `apps/kanidm/`, `apps/oauth2-proxy/` |
| Tailscale operator (任意) | 統合開発環境からの任意経路 (経路 B) | `apps/tailscale-operator/` |

`apps/devplatform-db` (ブランチ専用データベース) も併せて同期する。

## 利用者が用意するもの

外部のアカウント・契約がないと発行できないもの**だけ**を Infisical の
`prod` 環境 (`values.yaml` の `infisical.projectId` / `environmentSlug` /
`secretPath`) に登録する。値をリポジトリへ置いてはならない (14.4/14.5)。

| Infisical のキー | 中身 | 必須 |
|---|---|---|
| `DEVPLATFORM_WORKSPACE_CLAUDE_CODE_CREDENTIALS` | Claude Code の長期認証情報 (`credentials.json` の内容そのまま) | 必須 |
| `DEVPLATFORM_INFERENCE_API_KEY` | 推論バックエンドの API キー (Claude Code のサブスクリプション認証とは別系統) | 必須 |
| `TAILSCALE_OPERATOR_OAUTH_CLIENT_ID` | tailnet 所有者が発行する OAuth クライアント | 任意経路のみ |
| `TAILSCALE_OPERATOR_OAUTH_CLIENT_SECRET` | 同上 | 任意経路のみ |

秘匿情報以外に、配備先に合わせて `values.yaml` で調整するもの:

- `gateway.domain` / `gateway.apiHost` — ワイルドカード証明書を持つゾーンと API のホスト名
- `gateway.oidc.*` — 認可サーバの issuer / audience / JWKS URL
- `controller.nodeSelector` / `workspaceTemplate.nodeName` — ワークスペースの配置ノード
- `workspaceQuota.*` / `workspaceTemplate.storage.nodeDiskBudget` — クラスタの実容量に合わせた上限
- `controller.image` / `gateway.image` / `workspaceImage` — 自分でビルドしたイメージを使う場合

## 自動で用意されるもの

以下は外部に発行元が存在しないため、配備時にクラスタ内で発行される。手作業は不要。
いずれも**既にあれば何もしない**ため、同期を繰り返しても値は変わらない。

| 資格情報 | 発行する主体 | 置き場所 |
|---|---|---|
| SSH 認証局の秘密鍵 | Terminal Gateway が起動時に生成 | `devplatform/devplatform-gateway-ssh-ca` (`id_ca`) |
| SSH 認証局の公開鍵 | Terminal Gateway が秘密鍵から導出 | `devplatform-workspaces/devplatform-workspace-ssh-ca` (`ca.pub`) |
| 退避先 S3 の資格情報 | `devplatform-evacuation-credentials` Job が Garage の鍵を発行 | `devplatform-workspaces/devplatform-workspace-evacuation` (`access-key` / `secret-key`) |
| データベースの superuser 資格情報 | CloudNativePG が bootstrap 時に生成 | `devplatform-db/devplatform-db-cluster-superuser` |

これらの Secret は chart がレンダリングしない。Argo CD は `helm template` で
マニフェストを生成するため Helm の `lookup` がクラスタを参照できず、
「既にあれば維持する」をテンプレートで表現できないからである。
テンプレートに置けば同期のたびに鍵が置き換わり、発行済みの SSH 証明書が失効し、
退避データへ到達できなくなる。生成された Secret は Argo CD の管理対象外
(追跡ラベルを持たない) であり、加えて `argocd.argoproj.io/sync-options:
Prune=false,Delete=false` を付けて一律の prune/selfHeal から二重に守っている。

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
  併せて `workspaceTemplate.evacuation.bucket` と、controller が持つエンドポイント
  (`resources.go` の `garageS3Endpoint`) を配備先に合わせる。
- **データベース**: `apps/devplatform-db/cluster.yaml` に `superuserSecret` を書けば
  CloudNativePG の生成をやめて指定の Secret を使う。

## 配備手順

1. 上表の依存を配備する。
2. Infisical の `prod` 環境に、利用者が用意するキー (最小 2 個) を登録する。
3. `values.yaml` を配備先に合わせて調整する。
4. `apps/devplatform` と `apps/devplatform-db` を push する。ApplicationSet が
   ディレクトリ名の Application と同名 namespace を自動生成する。
5. `./scripts/validate-manifests.sh` でレンダリングを確認する (pre-commit hook からも実行される)。

同期後、`devplatform-gateway` Pod のログに SSH 認証局に関するエラーが出ていないこと、
`devplatform-workspaces` ネームスペースに `devplatform-workspace-ssh-ca` と
`devplatform-workspace-evacuation` が生成されていることを確認する。
証明書発行だけが利用できない状態 (503) でも、ブラウザからワークスペースへ到達する
必須経路は動作する。

## イメージ

`controller/`, `gateway/`, `image/workspace/` の Dockerfile が生成する 3 つのイメージを
使う。本リポジトリのリモートでは CI が稼働していないため、ビルドと push は手動運用とし、
タグは日付 + 短縮コミット SHA のような一意値のみを使う (`latest` を使わない)。
イメージは環境中立で、認証局・ホスト名・利用者固有の値を焼き込まない。
