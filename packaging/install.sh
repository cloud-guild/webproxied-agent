#!/bin/sh
# WebProxied Agent の導入スクリプト（WebProxiedのサーバーの画面に表示されるコマンドで実行する）。
#   curl -fsSL <WebProxiedのURL>/agent/install.sh | sudo sh -s -- --api-url <WebProxiedのURL>
# トークンは、実行中に貼り付ける（コマンドの履歴・プロセスの一覧に残さないため）。自動の導入では、--token-file <ファイル> でも渡せる。
# 実行ファイルは GitHub Releases から取得し、当社の署名（ed25519）を検証してから入れる（OpenSSL 3.0以降が必要）。
# オプション:
#   --read-logs            アクセスログ（5xxの割合）を読めるよう、エージェントのユーザーをログのグループ（adm）に加える
#                          （adm のグループは、syslog・auth.log・journal なども読める）
#   --allow-insecure-http  開発環境だけ。--api-url に http:// を許す（トークンが暗号化されずに送られる）
set -eu

# 途中までしか取得できなかったスクリプトを実行しないよう、全体を関数にして最後に呼ぶ
main() {
REPO="cloud-guild/webproxied-agent"
API_URL=""
TOKEN=""
VERSION="latest"
SKIP_SIGNATURE=0
ALLOW_INSECURE_HTTP=false
READ_LOGS=0
PREFIX="/opt/webproxied-agent"
CONFIG_DIR="/etc/webproxied-agent"

# 署名を検証する公開鍵（scripts/gen-key.sh の出力のPEM）。鍵を作るまでは空で、署名の検証ができないため導入しない。
PUBLIC_KEY_PEM=""

while [ $# -gt 0 ]; do
    case "$1" in
        --token-file) TOKEN="$(tr -d ' \r\n' < "$2")"; shift 2 ;;
        --api-url) API_URL="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --skip-signature-check) SKIP_SIGNATURE=1; shift ;;
        --allow-insecure-http) ALLOW_INSECURE_HTTP=true; shift ;;
        --read-logs) READ_LOGS=1; shift ;;
        *) echo "不明な引数: $1" >&2; exit 2 ;;
    esac
done

fail() { echo "エラー: $*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || fail "root（sudo）で実行してください"
[ -n "$API_URL" ] || fail "--api-url を指定してください"
case "$API_URL" in
    https://*) ;;
    http://*)
        [ "$ALLOW_INSECURE_HTTP" = "true" ] || fail "--api-url は https:// で始まるURLにしてください（開発環境で http:// を使うときだけ、--allow-insecure-http を付けます）"
        echo "注意: http:// で接続します（--allow-insecure-http）。トークンが暗号化されずに送られるため、開発環境だけで使ってください。" >&2
        ;;
    *) fail "--api-url は https:// で始まるURLにしてください" ;;
esac
command -v systemctl >/dev/null 2>&1 || fail "systemdが必要です"
command -v curl >/dev/null 2>&1 || fail "curlが必要です"

case "$(uname -s)" in Linux) ;; *) fail "Linuxだけに対応しています" ;; esac
case "$(uname -m)" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) fail "対応していないCPUです: $(uname -m)" ;;
esac

if [ -z "$TOKEN" ]; then
    # 端末の無い環境（cloud-init・SSM Run Command・Ansible・ptyの無いsudo）では、/dev/tty が読めても書き込めない
    if ! [ -r /dev/tty ] || ! ( : > /dev/tty ) 2>/dev/null; then
        fail "端末が無いため、トークンを入力できません（--token-file <ファイル> でトークンを渡してください）"
    fi
    printf 'WebProxiedのサーバーの画面に表示されたトークンを貼り付けて、Enterを押してください: ' > /dev/tty
    stty -echo < /dev/tty 2>/dev/null || true
    IFS= read -r TOKEN < /dev/tty || true
    stty echo < /dev/tty 2>/dev/null || true
    echo > /dev/tty
    TOKEN="$(printf '%s' "$TOKEN" | tr -d ' \r\n')"
fi
case "$TOKEN" in wpa_*) ;; *) fail "トークンの形式が違います（wpa_ で始まります）" ;; esac

ASSET="webproxied-agent_linux_${ARCH}"
if [ "$VERSION" = "latest" ]; then
    BASE="https://github.com/${REPO}/releases/latest/download"
else
    BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "取得しています: ${ASSET}"
curl -fsSL --proto '=https' -o "$WORK/$ASSET" "$BASE/$ASSET"
curl -fsSL --proto '=https' -o "$WORK/checksums.txt" "$BASE/checksums.txt"
curl -fsSL --proto '=https' -o "$WORK/checksums.txt.sig" "$BASE/checksums.txt.sig"

if [ "$SKIP_SIGNATURE" = "1" ]; then
    echo "注意: 署名を検証しません（--skip-signature-check）。配布元が改ざんされていても気づけません。" >&2
else
    [ -n "$PUBLIC_KEY_PEM" ] || fail "署名を検証する公開鍵が設定されていません"
    command -v openssl >/dev/null 2>&1 || fail "opensslが必要です（署名の検証）"
    # ed25519の署名の検証（-rawin）は、OpenSSL 3.0以降で使える
    if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then
        fail "このサーバーのOpenSSL（$(openssl version)）では、署名を検証できません（OpenSSL 3.0以降が必要です）。OpenSSL 3.0以降のある別のマシンで checksums.txt の署名を検証したうえで、--skip-signature-check を付けて実行してください。"
    fi
    printf '%s\n' "$PUBLIC_KEY_PEM" > "$WORK/release.pub.pem"
    if ! openssl pkeyutl -verify -pubin -inkey "$WORK/release.pub.pem" -rawin -in "$WORK/checksums.txt" -sigfile "$WORK/checksums.txt.sig" >/dev/null 2>&1; then
        fail "署名を検証できませんでした。配布物が改ざんされている可能性があるため、導入を中止しました。WebProxiedのサポートへお問い合わせください。"
    fi
    echo "署名を検証しました"
fi

EXPECTED="$(grep " ${ASSET}\$" "$WORK/checksums.txt" | awk '{print $1}')"
ACTUAL="$(sha256sum "$WORK/$ASSET" | awk '{print $1}')"
[ -n "$EXPECTED" ] && [ "$EXPECTED" = "$ACTUAL" ] || fail "SHA-256が一致しません"

# 専用のユーザーとグループ
getent group webproxied-agent >/dev/null 2>&1 || groupadd --system webproxied-agent 2>/dev/null || groupadd -r webproxied-agent
if ! id webproxied-agent >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir "$PREFIX" --shell /usr/sbin/nologin -g webproxied-agent webproxied-agent 2>/dev/null \
        || useradd -r -M -d "$PREFIX" -s /sbin/nologin -g webproxied-agent webproxied-agent
fi

# アクセスログを読むためのグループ（--read-logs を付けたときだけ加える）。
# 付けずに入れ直したときは、前に --read-logs で加えていた場合だけ外す（管理者が手で加えたものは外さない）
LOG_GROUP="adm"
READ_LOGS_CONF=false
READ_LOGS_BEFORE=0
if [ -r "$CONFIG_DIR/agent.conf" ] && grep -qx 'READ_LOGS=true' "$CONFIG_DIR/agent.conf"; then
    READ_LOGS_BEFORE=1
fi
if [ "$READ_LOGS" = "1" ]; then
    READ_LOGS_CONF=true
    if getent group "$LOG_GROUP" >/dev/null 2>&1; then
        usermod -a -G "$LOG_GROUP" webproxied-agent
        echo "アクセスログを読めるよう、webproxied-agent を ${LOG_GROUP} のグループに加えました（${LOG_GROUP} のグループは、syslog・auth.log・journal なども読めます）"
    else
        echo "注意: ${LOG_GROUP} のグループが無いため、グループに加えませんでした（ログのファイルの権限を確認してください）" >&2
    fi
    if [ -d /var/log/httpd ]; then
        echo "注意: RHEL系の httpd のログ（/var/log/httpd）は、rootだけが読めるため、${LOG_GROUP} のグループでは読めません" >&2
    fi
elif [ "$READ_LOGS_BEFORE" = "1" ] && id -nG webproxied-agent 2>/dev/null | tr ' ' '\n' | grep -qx "$LOG_GROUP"; then
    gpasswd -d webproxied-agent "$LOG_GROUP" >/dev/null
    echo "--read-logs を付けていないため、webproxied-agent を ${LOG_GROUP} のグループから外しました"
fi

# 実行ファイルは、自動更新で置き換えられるよう、エージェントのユーザーが書ける場所に置く
install -d -o webproxied-agent -g webproxied-agent -m 0755 "$PREFIX"
rm -f "$PREFIX/webproxied-agent.new" "$PREFIX/webproxied-agent.update-pending"
install -o webproxied-agent -g webproxied-agent -m 0755 "$WORK/$ASSET" "$PREFIX/webproxied-agent"

install -d -m 0750 -g webproxied-agent "$CONFIG_DIR"
umask 027
cat > "$CONFIG_DIR/agent.conf" <<CONF
# WebProxied Agent の設定（トークンを含むため、他の人に見せないでください）
API_URL=${API_URL}
TOKEN=${TOKEN}
INTERVAL_SECONDS=60
AUTO_UPDATE=true
ALLOW_INSECURE_HTTP=${ALLOW_INSECURE_HTTP}
# --read-logs で ${LOG_GROUP} のグループに加えたか（入れ直しのときに使う）
READ_LOGS=${READ_LOGS_CONF}
CONF
chgrp webproxied-agent "$CONFIG_DIR/agent.conf"
chmod 0640 "$CONFIG_DIR/agent.conf"

# systemdのサービス（packaging/webproxied-agent.service と同じ内容）
cat > /etc/systemd/system/webproxied-agent.service <<'UNIT'
[Unit]
Description=WebProxied Agent
Documentation=https://github.com/cloud-guild/webproxied-agent
After=network-online.target
Wants=network-online.target

[Service]
User=webproxied-agent
Group=webproxied-agent
ExecStart=/opt/webproxied-agent/webproxied-agent run
# 自動更新で実行ファイルを置き換えたら、終了して再起動する（更新した版が送信に成功しなければ、3回目の起動で元の版へ戻す）
Restart=always
RestartSec=10
# 読み取りだけを行う。書き込めるのは、エージェントの実行ファイルの場所だけ
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
LockPersonality=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
CapabilityBoundingSet=
ReadWritePaths=/opt/webproxied-agent

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload

echo "WebProxiedへ接続できるか確かめています"
if command -v runuser >/dev/null 2>&1; then
    runuser -u webproxied-agent -- "$PREFIX/webproxied-agent" once || fail "WebProxiedへ送れませんでした。トークンとネットワーク（${API_URL} への接続）を確認してください"
else
    su -s /bin/sh webproxied-agent -c "$PREFIX/webproxied-agent once" || fail "WebProxiedへ送れませんでした。トークンとネットワーク（${API_URL} への接続）を確認してください"
fi

# 入れ直し（トークンの作り直し・版の更新）でも、新しい設定と実行ファイルで動くよう、再起動する
systemctl enable webproxied-agent >/dev/null 2>&1
systemctl restart webproxied-agent
echo "導入しました。WebProxiedのサーバーの画面で、接続できたことを確認してください。"
}

main "$@"
