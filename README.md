# WebProxied Agent

[WebProxied](https://webproxied.com) のエージェントです。サーバーに入れて常駐させ、サーバーの情報を集めて、WebProxiedへHTTPSで送ります。

- **読み取りだけ**を行います。サーバーの設定の変更や、コマンドの実行の機能はありません。
- データは、エージェントからWebProxiedへ送ります（サーバー側で、受信用のポートを開ける必要はありません）。
- 対応：Linux（amd64・arm64）、systemd。Ubuntu 20.04以降・Debian 11以降・RHEL／Rocky Linux／AlmaLinux 8以降・Amazon Linux 2／2023。

## 導入

WebProxiedのサーバーの画面に表示されるコマンドを実行し、表示されたトークンを貼り付けます（トークンは、コマンドの履歴やプロセスの一覧に残らないよう、実行中に入力します）。

```sh
curl -fsSL https://webproxied.com/agent/install.sh | sudo sh -s -- --api-url https://webproxied.com
```

- 導入スクリプトは、WebProxiedのドメインから配布します（このリポジトリの `packaging/install.sh` と同じ内容）。
- 入れ直し（トークンの作り直し・版の更新）でも、同じコマンドを実行します。
- 実行ファイルは GitHub Releases から取得し、当社の署名（ed25519）を検証してから入れます（OpenSSL 3.0以降が必要です）。
- OpenSSL 3.0より古いサーバー（Amazon Linux 2・RHEL 8など）では、署名を検証できません。別のマシンで署名を検証したうえで、`--skip-signature-check` を付けて実行してください。
- 専用のユーザー `webproxied-agent` で動きます。実行ファイルは `/opt/webproxied-agent/`、設定は `/etc/webproxied-agent/agent.conf` です。

## 自動更新

エージェントは1日1回、新しい版を確かめ、**当社の署名を検証できたときだけ**自分で更新します。配布元（GitHub）が乗っ取られても、署名の秘密鍵はGitHubとは別の場所で保管しているため、偽の版へは更新しません。

- 止めるには、設定の `AUTO_UPDATE=false`、またはWebProxiedの画面で自動更新をオフにします。
- 手動で更新するには `sudo -u webproxied-agent /opt/webproxied-agent/webproxied-agent update` のあと、`sudo systemctl restart webproxied-agent`。

## 削除

```sh
sudo systemctl disable --now webproxied-agent
sudo rm -rf /opt/webproxied-agent /etc/webproxied-agent /etc/systemd/system/webproxied-agent.service
sudo userdel webproxied-agent
```

## リリース（開発者向け）

1. `v1.2.3` の形のタグを付けて push すると、GitHub Actionsが実行ファイルと `checksums.txt` を作り、下書きのリリースにします。
2. 秘密鍵を持つ人が、手元で `sh scripts/sign-release.sh v1.2.3 <秘密鍵>` を実行します。手元で同じ版を作り直してハッシュが一致することを確かめてから、署名を添付して公開します。
3. 鍵の作り方は `scripts/gen-key.sh`（公開鍵を `internal/update/keys/release.pub` と `packaging/install.sh` に入れる）。

## 開発

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.24-bookworm go test ./...
```
