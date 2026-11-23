# WebProxied Agent

[WebProxied](https://webproxied.com) のエージェントです。サーバーに入れて常駐させ、サーバーの情報を集めて、WebProxiedへHTTPSで送ります。

- **読み取りだけ**を行います。サーバーの設定の変更や、コマンドの実行の機能はありません。
- 専用のユーザー `webproxied-agent` で動き、そのユーザーの権限で読めるものだけを読みます。systemd の制限で、`/opt/webproxied-agent` のほかには書き込めません（`/home`・`/root` は読み取りだけで見えます。`/root` は通常 root だけが読めるため、読めません）。
- データは、エージェントからWebProxiedへ送ります（サーバー側で、受信用のポートを開ける必要はありません）。
- 対応：Linux（amd64・arm64）、systemd。Ubuntu 20.04以降・Debian 11以降・RHEL／Rocky Linux／AlmaLinux 8以降・Amazon Linux 2／2023。

## 導入

WebProxiedのサーバーの画面に表示されるコマンドを実行し、表示されたトークンを貼り付けます（トークンは、コマンドの履歴やプロセスの一覧に残らないよう、実行中に入力します）。

```sh
curl -fsSL https://webproxied.com/agent/install.sh | sudo sh -s -- --api-url https://webproxied.com
```

- 導入スクリプトは、WebProxiedのドメインから配布します（このリポジトリの `packaging/install.sh` と同じ内容）。
- 入れ直し（トークンの作り直し・版の更新）でも、同じコマンドを実行します。
  - 同じ `--api-url` で入れ直すときは、トークンを聞かれたら何も入れずにEnterを押すと、今の設定のトークンを使い続けます（トークンを作り直さずに済みます）。
  - 新しいトークンを貼り付ければ、そのトークンに替わります。端末が無く `--token-file` も無いときは、今のトークンを使い続けます。
  - 送り先（`--api-url`）が違うときは、今のトークンを使いません（別の送り先へ送らないため）。
- 実行ファイルは GitHub Releases から取得し、当社の署名（ed25519）を検証してから入れます（OpenSSL 3.0以降が必要です）。
- OpenSSL 3.0より古いサーバー（Amazon Linux 2・RHEL 8など）では、署名を検証できません。別のマシンで署名を検証したうえで、`--skip-signature-check` を付けて実行してください。
- 専用のユーザー `webproxied-agent` で動きます。実行ファイルは `/opt/webproxied-agent/`、設定は `/etc/webproxied-agent/agent.conf` です。

### 導入のオプション

- `--no-read-logs`：アクセスログの読み取りを足しません（下の「アクセスログの読み取り」）。入れ直しで付けると、前にこのスクリプトが足したものだけを外します。
- `--read-logs`：既定と同じです（前の版のコマンドのために受け付けます）。
- `--allow-insecure-http`：開発環境だけで使います。`--api-url` に `http://` を許します（トークンが暗号化されずに送られます）。
- `--token-file <ファイル>`：端末の無い環境（cloud-init・SSM Run Command・Ansibleなど）では、トークンをファイルで渡します。

### アクセスログの読み取り

アクセスログ（5xxの割合）を読めるよう、導入スクリプトは、Webサーバーのログのディレクトリ（`/var/log/nginx`・`/var/log/apache2`・`/var/log/httpd` のうち、あるもの）に、エージェントのユーザーの読み取りだけを ACL で足します。

- ディレクトリに読み取りと通過、中のファイルに読み取りを足します。ログを作り直したとき（logrotate）のファイルにも引き継ぐよう、デフォルトACL も付けます。
- 書き込みや、ほかのログ（syslog・auth.log など）の読み取りは増えません。ファイルの権限（マスク）を超えて読めるようにはしません。
- 足したディレクトリは、`agent.conf` の `LOG_ACL_DIRS` に記録します。入れ直し・削除のときは、記録したものだけを外します（管理者が足した ACL は触りません）。
- ディレクトリが無いとき（DB・踏み台など）は、何も足しません。シンボリックリンクのディレクトリも触りません。
- `setfacl` が無い、または ACL を使えないときは、何も足さずに案内します（`acl` のパッケージを入れてから、実行し直します）。
- 前の版の `--read-logs` で `adm` のグループに加えていた場合は、ACL に置き換えて、`adm` から外します（`adm` は syslog・auth.log なども読めて広いため）。ACL を使えないときは、今の読み取りを残します。
- 最後に、何を足したかを表示します。

## 集める情報

すべて読み取りだけで、実行するコマンドは決まったもの（シェルを通さず、`/usr/sbin` などの決まった場所から探す）だけです。時間と出力の量に上限を設けています。

- **毎回（1分ごと）**：CPU（使用率・steal・iowait）・ロードアベレージ・メモリ・スワップ・ディスク（tmpfs・overlay・squashfsなどを除く）・スレッドの数と上限・時刻の同期（chronyc・timedatectl）・OOMの回数・稼働時間。
- **毎回（1分ごと）**：インターフェースのIPアドレス（IPv4・IPv6。最大32件）。パブリック・プライベートの判断は、WebProxiedが行います。
  - up のインターフェースだけです。ループバック・リンクローカル、IPv6の一時的・確認中・非推奨のアドレスは送りません。
  - コンテナ・仮想ネットワーク・VPNのインターフェース（`docker*`・`br-*`・`veth*`・`virbr*`・`cni*`・`flannel*`・`cali*`・`vxlan*`・`tun*`・`tap*`・`wg*`・`kube-*`・`cilium*`・`lxc*`・`lxd*`・`podman*`・`weave*`・`dummy*`・`nerdctl*`・`tailscale*`・`zt*`）は送りません。
  - サービスは netlink を使えない（`RestrictAddressFamilies`）ため、IPv4は `ioctl`（`SIOCGIFCONF`）と `/proc/net/route`、IPv6は `/proc/net/if_inet6` で読みます。
  - EC2 のパブリックIPのように、NAT でサーバーの外で付くアドレスは、サーバーからは見えません。
- ディスクは、`/home` が別のパーティションなら、その容量も送ります。
- **WebProxiedの画面で指定したとき**：サービスの状態（`systemctl show`、最大20個）、アクセスログ（`/var/log/` の下だけ。前回から後に追記された行の件数と5xxの件数。ログの入れ替えにも対応）。
- **起動の直後（最初の応答を受けた後）と、WebProxiedが求めたとき**：入っているパッケージ（dpkg・rpm・apk、最大2万件）・ミドルウェアの版（nginx・apache・php・mysql/mariadb・postgresql・redis・node）・サービスの一覧。
  - WebProxiedの画面でアプリケーションのディレクトリを指定していれば、そのライブラリ・フレームワーク・CMSの版も送ります（下の「アプリケーションのライブラリ」）。
  - SSHの設定（ログインに関わる4つ）と、再起動を待っている更新も送ります（下の「SSHの設定と再起動待ちの更新」）。

## アプリケーションのライブラリ

WebProxiedの画面で指定したディレクトリ（最大10個）の**直下**にある次のファイルから、パッケージの名前と版だけを読みます。ファイルの中身そのもの（`composer.json` の認証情報など）は送りません。

| 読むもの | 送るもの |
|---|---|
| `composer.lock`（`composer.json` で直接の依存を判断） | Composer のパッケージ（packagist） |
| `package-lock.json`（lockfileVersion 2・3）・`yarn.lock`（v1・berry）・`pnpm-lock.yaml`（lockfileVersion 6〜9）（`package.json` で直接の依存を判断） | npm のパッケージ |
| `wp-includes/version.php`・`wp-content/plugins/<スラッグ>/` の主ファイル・`wp-content/themes/<スラッグ>/style.css` | WordPress の本体・プラグイン・テーマの版（見出しの先頭8KBだけ読む） |

- 指定したディレクトリ自体のシンボリックリンク（`current` → `releases/…` など）は辿ります。その中では、ディレクトリの外を指すリンクは辿りません。
- 上限：1つのファイルは10MBまで、1つのディレクトリで5,000件・全体で20,000件まで、WordPressのプラグイン・テーマは2,000個まで。
- 読めないときは、そのディレクトリに理由（`not_found`・`permission_denied`・`parse_error:<ファイル>`・`too_large:<ファイル>`・`unsupported:<ファイル>` など）を付けて送り、読めたものは送ります。
- 対応していないもの：`package-lock.json` の lockfileVersion 1（npm 6まで）、pnpm-lock.yaml の lockfileVersion 5 以前、`npm-shrinkwrap.json`、サブディレクトリにある WordPress（Bedrock の `web/wp` など。`composer.lock` に出る `roots/wordpress`・`wpackagist-plugin/*` は送ります）。`yarn.lock`・`pnpm-lock.yaml`（v9）には開発用の区別が無いため、`dev` は `package.json` の devDependencies だけにある直接の依存だけに付きます。
- `/home` の下（KUSANAGI の `/home/kusanagi/<名前>/DocumentRoot` など）も読めます（読み取りだけ。専用のユーザーの権限の範囲で）。

### 読めないとき（permission_denied）

エージェントは専用のユーザー `webproxied-agent` で動くため、そのユーザーが読める必要があります。一般的な `/var/www` の権限（ディレクトリ755・ファイル644）なら、そのまま読めます。読めないときは、**読むファイルと、そこまでのディレクトリにだけ**権限を与えてください（`wp-config.php` や `.env` などに読み取りの権限を与えないでください）。ACL を使う例：

```sh
# ディレクトリの中に入れるようにする（一覧は見せない）
sudo setfacl -m u:webproxied-agent:x /var/www /var/www/example /var/www/example/current
# ロックファイルなどを読めるようにする
cd /var/www/example/current
sudo setfacl -m u:webproxied-agent:r composer.lock composer.json package.json package-lock.json
# WordPress：版のファイルと、プラグイン・テーマの見出し（一覧を見るため、plugins・themes には r と x）
sudo setfacl -m u:webproxied-agent:x wp-includes wp-content
sudo setfacl -m u:webproxied-agent:r wp-includes/version.php
# プラグイン・テーマ：主ファイルを探すため、中のディレクトリの一覧と .php・style.css を読めるようにする
sudo setfacl -R -m u:webproxied-agent:rX wp-content/plugins wp-content/themes
```

WordPress の標準の権限（ディレクトリ755・ファイル644）なら、これらは不要です。

## SSHの設定と再起動待ちの更新

### SSHの設定

sshd の設定ファイル（`/etc/ssh/sshd_config` と、`Include` で読み込むファイル）を読み、次の4つの、実際に効いている値を送ります。`sshd -T` は使いません（root が要るため）。

- `PermitRootLogin`・`PasswordAuthentication`・`KbdInteractiveAuthentication`（古い名前の `ChallengeResponseAuthentication` も同じ設定として読みます）・`UsePAM`
- sshd と同じ順で読みます。最初に現れた値が勝ち、`Include` はその場所で展開します。どこにも書かれていなければ、OpenSSH の既定の値です。
- `Match` の中の値は、別に送ります（特定のユーザー・接続元だけ許可している場合に気づけるよう）。`Match all` はすべての接続に効くため、その値を、効いている値とします。
- Ubuntu 24.04・Debian 12・Rocky Linux 9・AlmaLinux 8・Amazon Linux 2023 で、`sshd -T` と同じ値になることを確かめています。
- 読めないファイルがあれば、値を送らず、理由（`permission_denied:<ファイル>` など）を送ります。WebProxiedの画面では「確認できず」になります。
- sshd が入っていなければ、そのことだけを送ります。
- systemd のユニットなどで、sshd に `-f`・`-o` を渡して設定を変えている場合は、反映されません。

#### RHEL系で読めるようにする（任意）

RHEL・Rocky Linux・AlmaLinux・Amazon Linux では、`/etc/ssh/sshd_config` と `/etc/ssh/sshd_config.d/` が root だけ読める権限（0600・0700）のため、そのままでは「確認できず」になります。読めるようにするには、エージェントのユーザーに読み取りだけを足します（書き込みや、ほかのファイルの読み取りは増えません）。

```sh
sudo setfacl -m u:webproxied-agent:r /etc/ssh/sshd_config
# RHEL 9・Amazon Linux 2023 など、sshd_config.d がある場合
sudo setfacl -m u:webproxied-agent:rx /etc/ssh/sshd_config.d
sudo setfacl -m u:webproxied-agent:r /etc/ssh/sshd_config.d/*.conf
# すぐに画面へ反映する（サーバーの情報は、起動の後と、WebProxiedが求めたときに送るため）
sudo systemctl restart webproxied-agent
```

- 導入スクリプトは、エージェントのユーザーが読めないときに、このサーバーに合わせたコマンドを表示します（権限は変えません）。

- `openssh-server` を更新すると、権限が元に戻り、足した読み取りは消えます（Rocky Linux 9 で確かめました）。更新の後は、もう一度実行してください。読めなくなると、画面に「確認できず」と出るので気づけます。
- 後から `sshd_config.d/` に足したファイルにも、同じように読み取りを足してください。

### 再起動を待っている更新

- Debian・Ubuntu：`/run/reboot-required` の有無と、`/run/reboot-required.pkgs` のパッケージの名前（最大100件）。
- RHEL系・Amazon Linux：`dnf needs-restarting -r` と同じ考え方で、カーネル・glibc・systemd・openssl-libs などの中心のパッケージが、起動の後に入ったかを、`rpm` の入れた時刻と起動の時刻で比べます（`needs-restarting` は実行しません）。
- Alpine Linux などは、対象外（`unsupported`）と送ります。

## 自動更新

エージェントは1日1回、新しい版を確かめ、**当社の署名を検証できたときだけ**自分で更新します。配布元（GitHub）が乗っ取られても、署名の秘密鍵はGitHubとは別の場所で保管しているため、偽の版へは更新しません。

- 止めるには、設定の `AUTO_UPDATE=false`、またはWebProxiedの画面で自動更新をオフにします。
- 自動更新で置き換わるのは、実行ファイルだけです。systemd のサービスの設定（`/etc/systemd/system/webproxied-agent.service`）が変わった版では、導入のコマンドを実行し直すと反映されます（トークンを聞かれたら、何も入れずにEnterで、今のトークンを使い続けます）。
- 手動で更新するには `sudo -u webproxied-agent /opt/webproxied-agent/webproxied-agent update` のあと、`sudo systemctl restart webproxied-agent`。

## 削除

```sh
curl -fsSL https://webproxied.com/agent/install.sh | sudo sh -s -- --uninstall
```

- サービスを止めて、`/opt/webproxied-agent`・`/etc/webproxied-agent`・サービスの定義・ユーザー `webproxied-agent` を消します。
- ログのディレクトリ（`/var/log/nginx`・`apache2`・`httpd`）から、`webproxied-agent` の ACL（デフォルトACL も）を外します。ディレクトリや ACL が無くても止まりません。

手で消すときは、次のとおりです（ACL の行は、ユーザーを消す前に実行します）。

```sh
sudo systemctl disable --now webproxied-agent
for d in /var/log/nginx /var/log/apache2 /var/log/httpd; do [ -d "$d" ] && command -v setfacl >/dev/null && { sudo setfacl -R -P -x u:webproxied-agent "$d"; sudo setfacl -x d:u:webproxied-agent "$d"; } 2>/dev/null; done; true
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
