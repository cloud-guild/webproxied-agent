# WebProxied Agent

[WebProxied](https://webproxied.com) のエージェントです。サーバーに常駐し、サーバーの情報を集めて、WebProxiedへHTTPSで送ります。

- **読み取りだけ**を行います。サーバーの設定を変えたり、コマンドを実行したりする機能はありません。
- 専用のユーザー `webproxied-agent` で動き、そのユーザーが読めるものだけを読みます。書き込めるのは `/opt/webproxied-agent` だけです。
- エージェントからWebProxiedへ送るだけなので、サーバーで受信用のポートを開ける必要はありません。
- 対応：Linux（amd64・arm64）、systemd、OpenSSL 3.0以降。Ubuntu 22.04以降・Debian 12以降・RHEL／Rocky Linux／AlmaLinux 9以降・Amazon Linux 2023。
  - RHEL系 8 では、EPEL の `openssl3` を入れれば使えます（`sudo dnf install epel-release && sudo dnf install openssl3`）。

## 導入

WebProxiedのサーバーの画面に表示されるコマンドを実行し、画面のトークンを貼り付けます。

```sh
curl -fsSL https://webproxied.com/agent/install.sh | sudo sh
```

- 実行ファイルは GitHub Releases から取得し、当社の署名を検証してから入れます。
- 入れ直し（版の更新・トークンの作り直し）も、同じコマンドです。トークンを聞かれたら、何も入れずにEnterで、今のトークンを使い続けます。
- オプション：
  - `--token-file <ファイル>`：端末の無い環境（cloud-init・SSM Run Command・Ansibleなど）で、トークンをファイルで渡します。
  - `--api-url <URL>`：送り先を変えます（開発環境用）。`http://` は、localhost・プライベートIP・`.test` などの開発環境の送り先だけで使えます。トークンが暗号化されずに送られるため、注意してください。

## 削除

```sh
curl -fsSL https://webproxied.com/agent/install.sh | sudo sh -s -- --uninstall
```

サービス・ファイル・ユーザーと、導入で足したアクセスログの読み取りの権限を消します。

## 集める情報

| いつ | 何を |
|---|---|
| 1分ごと | CPU・ロードアベレージ・メモリ・スワップ・ディスク・プロセスの数・時刻の同期・OOMの回数・稼働時間・IPアドレス |
| WebProxiedの画面で指定したとき | サービスの状態、アクセスログの件数と5xxの件数 |
| 起動の後と、WebProxiedが求めたとき | パッケージの一覧・ミドルウェアの版・サービスの一覧・SSHの設定・再起動を待っている更新、画面で指定したディレクトリのアプリケーションのライブラリ（Composer・npm・WordPress） |

- 実行するコマンドは決まったもの（`dpkg`・`rpm`・`systemctl` など）だけで、時間と出力の量に上限があります。
- ファイルの中身そのもの（`.env`・`wp-config.php` など）は送りません。ライブラリは、ロックファイルなどから名前と版だけを読みます。
- IPアドレスは、インターフェースに付いたものだけです（コンテナ・VPNのものは除きます）。EC2 のパブリックIPのように、サーバーの外で付くアドレスは見えません。

## 読めないとき

エージェントは専用のユーザーで動くため、そのユーザーが読めないものは「確認できず」になります。

- **アクセスログ**：導入スクリプトが、nginx・Apache のログのディレクトリ（`/var/log/nginx`・`apache2`・`httpd`）に、読み取りだけの権限を足します。ほかの場所のログは、読み取りの権限を与えてください。
- **SSHの設定（RHEL系）**：`/etc/ssh/sshd_config` などが root だけ読める権限のため、読めません。読めるようにするには、次を実行します（導入スクリプトも、読めないときに表示します）。

  ```sh
  sudo setfacl -m u:webproxied-agent:r /etc/ssh/sshd_config
  sudo setfacl -m u:webproxied-agent:rx /etc/ssh/sshd_config.d
  sudo setfacl -m u:webproxied-agent:r /etc/ssh/sshd_config.d/*.conf
  sudo systemctl restart webproxied-agent
  ```

  `openssh-server` を更新したときと、`sshd_config.d` にファイルを足したときは、もう一度実行してください。
- **アプリケーションのディレクトリ**：一般的な権限（ディレクトリ755・ファイル644）なら読めます。読めないときは、読むファイル（`composer.lock`・`package.json` など）と、そこまでのディレクトリにだけ権限を与えてください。`.env`・`wp-config.php` には与えないでください。

  ```sh
  sudo setfacl -m u:webproxied-agent:x /var/www /var/www/example
  sudo setfacl -m u:webproxied-agent:r /var/www/example/composer.lock /var/www/example/composer.json
  ```

## 自動更新

1日1回新しい版を確かめ、当社の署名を検証できたときだけ更新します。止めるには、WebProxiedの画面で自動更新をオフにします。

## 開発者向け

テスト：

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.24-bookworm go test ./...
```

リリース：

1. `v1.2.3` の形のタグを付けて push すると、GitHub Actions が下書きのリリースを作ります。
2. 秘密鍵を持つ人が `sh scripts/sign-release.sh v1.2.3 <秘密鍵>` を実行します。手元で作り直してハッシュが一致することを確かめてから、署名を添付して公開します。
3. 鍵は `scripts/gen-key.sh` で作ります（公開鍵は `internal/update/keys/release.pub` と `packaging/install.sh` に入れます）。
