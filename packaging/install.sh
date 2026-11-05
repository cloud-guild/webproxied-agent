#!/bin/sh
# WebProxied Agent の導入スクリプト。
#   curl -fsSL https://raw.githubusercontent.com/cloud-guild/webproxied-agent/main/packaging/install.sh | sudo sh -s -- --token <トークン> [--api-url https://webproxied.com]
# 実行ファイルは GitHub Releases から取得し、当社の署名（ed25519）を検証してから入れる（OpenSSL 3.0以降が必要）。
set -eu

REPO="cloud-guild/webproxied-agent"
API_URL="https://webproxied.com"
TOKEN=""
VERSION="latest"
SKIP_SIGNATURE=0
PREFIX="/opt/webproxied-agent"
CONFIG_DIR="/etc/webproxied-agent"

# 署名を検証する公開鍵（scripts/gen-key.sh の出力のPEM）。鍵を作るまでは空で、署名の検証ができないため導入しない。
PUBLIC_KEY_PEM=""

while [ $# -gt 0 ]; do
    case "$1" in
        --token) TOKEN="$2"; shift 2 ;;
        --api-url) API_URL="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --skip-signature-check) SKIP_SIGNATURE=1; shift ;;
        *) echo "不明な引数: $1" >&2; exit 2 ;;
    esac
done

fail() { echo "エラー: $*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || fail "root（sudo）で実行してください"
[ -n "$TOKEN" ] || fail "--token を指定してください（WebProxiedのサーバーの画面に表示されます）"
command -v systemctl >/dev/null 2>&1 || fail "systemdが必要です"
command -v curl >/dev/null 2>&1 || fail "curlが必要です"

case "$(uname -s)" in Linux) ;; *) fail "Linuxだけに対応しています" ;; esac
case "$(uname -m)" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) fail "対応していないCPUです: $(uname -m)" ;;
esac

ASSET="webproxied-agent_linux_${ARCH}"
if [ "$VERSION" = "latest" ]; then
    BASE="https://github.com/${REPO}/releases/latest/download"
else
    BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "取得しています: ${ASSET}"
curl -fsSL -o "$WORK/$ASSET" "$BASE/$ASSET"
curl -fsSL -o "$WORK/checksums.txt" "$BASE/checksums.txt"
curl -fsSL -o "$WORK/checksums.txt.sig" "$BASE/checksums.txt.sig"

if [ "$SKIP_SIGNATURE" = "1" ]; then
    echo "注意: 署名を検証しません（--skip-signature-check）。SHA-256だけを確かめます。" >&2
else
    [ -n "$PUBLIC_KEY_PEM" ] || fail "署名を検証する公開鍵が設定されていません"
    command -v openssl >/dev/null 2>&1 || fail "opensslが必要です（署名の検証）"
    printf '%s\n' "$PUBLIC_KEY_PEM" > "$WORK/release.pub.pem"
    if ! openssl pkeyutl -verify -pubin -inkey "$WORK/release.pub.pem" -rawin -in "$WORK/checksums.txt" -sigfile "$WORK/checksums.txt.sig" >/dev/null 2>&1; then
        fail "署名を検証できませんでした（OpenSSL 3.0以降が必要です。古いOpenSSLの場合は、内容を確かめたうえで --skip-signature-check を付けて実行してください）"
    fi
    echo "署名を検証しました"
fi

EXPECTED="$(grep " ${ASSET}\$" "$WORK/checksums.txt" | awk '{print $1}')"
ACTUAL="$(sha256sum "$WORK/$ASSET" | awk '{print $1}')"
[ -n "$EXPECTED" ] && [ "$EXPECTED" = "$ACTUAL" ] || fail "SHA-256が一致しません"

# 専用のユーザー（ログを読めるよう、adm・systemd-journalのグループがあれば加える）
if ! id webproxied-agent >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir "$PREFIX" --shell /usr/sbin/nologin webproxied-agent 2>/dev/null \
        || useradd -r -M -d "$PREFIX" -s /sbin/nologin webproxied-agent
fi
for group in adm systemd-journal; do
    if getent group "$group" >/dev/null 2>&1; then usermod -a -G "$group" webproxied-agent; fi
done

# 実行ファイルは、自動更新で置き換えられるよう、エージェントのユーザーが書ける場所に置く
install -d -o webproxied-agent -g webproxied-agent -m 0755 "$PREFIX"
install -o webproxied-agent -g webproxied-agent -m 0755 "$WORK/$ASSET" "$PREFIX/webproxied-agent"

install -d -m 0750 -g webproxied-agent "$CONFIG_DIR"
umask 027
cat > "$CONFIG_DIR/agent.conf" <<CONF
# WebProxied Agent の設定（トークンを含むため、他の人に見せないでください）
API_URL=${API_URL}
TOKEN=${TOKEN}
INTERVAL_SECONDS=60
AUTO_UPDATE=true
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
# 自動更新で実行ファイルを置き換えたら、終了して再起動する
Restart=always
RestartSec=10
# 読み取りだけを行う。書き込めるのは、エージェントの実行ファイルの場所だけ
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/opt/webproxied-agent

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload

echo "WebProxiedへ接続できるか確かめています"
if ! su -s /bin/sh webproxied-agent -c "$PREFIX/webproxied-agent once"; then
    fail "WebProxiedへ送れませんでした。トークンとネットワーク（${API_URL} へのHTTPS）を確認してください"
fi

systemctl enable --now webproxied-agent
echo "導入しました。WebProxiedのサーバーの画面で、接続できたことを確認してください。"
