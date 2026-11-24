#!/bin/sh
# WebProxied Agent の導入スクリプト（WebProxiedのサーバーの画面に表示されるコマンドで実行する）。
#   curl -fsSL https://webproxied.com/agent/install.sh | sudo sh
# トークンは、実行中に貼り付ける（コマンドの履歴・プロセスの一覧に残さないため）。自動の導入では、--token-file <ファイル> でも渡せる。
# 入れ直しで、同じ送り先のトークンが設定にあれば、何も入れずにEnterで、そのトークンを使い続ける（端末が無いときも、使い続ける）。
# 実行ファイルは GitHub Releases から取得し、当社の署名（ed25519）を検証してから入れる（OpenSSL 3.0以降が必要。openssl・openssl3 を探す）。
# アクセスログ（5xxの割合）を読めるよう、Webサーバーのログのディレクトリ（/var/log/nginx・apache2・httpd のうち、あるもの）に、
# エージェントのユーザーの読み取りだけを ACL で足す（ほかのログ・書き込みは増えない）。setfacl が無ければ、何も足さずに案内する。
# オプション:
#   --api-url <URL>          送り先（既定は https://webproxied.com）。http:// は、開発環境（localhost・プライベートIP・.test など）だけ許す
#   --token-file <ファイル>  トークンをファイルで渡す（端末の無い環境）
#   --version <vX.Y.Z>       入れる版（既定は最新）
#   --uninstall              エージェントを削除する（サービス・ファイル・ユーザーと、足したログの読み取り）
set -eu

# 途中までしか取得できなかったスクリプトを実行しないよう、全体を関数にして最後に呼ぶ
main() {
REPO="cloud-guild/webproxied-agent"
API_URL="https://webproxied.com"
TOKEN=""
VERSION="latest"
ALLOW_INSECURE_HTTP=false
UNINSTALL=0
PREFIX="/opt/webproxied-agent"
CONFIG_DIR="/etc/webproxied-agent"
AGENT_USER="webproxied-agent"
# 読み取りを足す、Webサーバーのログのディレクトリ（空白を含まない）
LOG_DIRS="/var/log/nginx /var/log/apache2 /var/log/httpd"

# 署名を検証する公開鍵（scripts/gen-key.sh の出力のPEM）。internal/update/keys/release.pub と同じ鍵。
PUBLIC_KEY_PEM="-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEArl9JH3xfWdg0KLAzFeRN85M1Ofg8uD3mMGaicD8bUJ0=
-----END PUBLIC KEY-----"

while [ $# -gt 0 ]; do
    case "$1" in
        --token-file) TOKEN="$(tr -d ' \r\n' < "$2")"; shift 2 ;;
        --api-url) API_URL="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --uninstall) UNINSTALL=1; shift ;;
        *) echo "不明な引数: $1" >&2; exit 2 ;;
    esac
done

fail() { echo "エラー: $*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || fail "root（sudo）で実行してください"

# 前にこのスクリプトが足したもの（入れ直し・削除のときに、それだけを外す）
LOG_ACL_BEFORE=""
if [ -r "$CONFIG_DIR/agent.conf" ]; then
    LOG_ACL_BEFORE="$(sed -n 's/^LOG_ACL_DIRS=//p' "$CONFIG_DIR/agent.conf" | tail -n 1 | tr -d "\"'\r")"
fi

if [ "$UNINSTALL" = "1" ]; then
    uninstall
    return 0
fi

case "$API_URL" in
    https://*) ;;
    http://*)
        # http:// は、開発環境の送り先だけ許す（本番のURLを打ち間違えて、トークンを暗号化せずに送らないため）
        dev_host "$API_URL" || fail "送り先（--api-url）は https:// で始まるURLにしてください（http:// は、localhost・プライベートIP・.test などの開発環境だけで使えます）"
        ALLOW_INSECURE_HTTP=true
        echo "注意: 開発環境として http:// で接続します。トークンが暗号化されずに送られます。" >&2
        ;;
    *) fail "送り先（--api-url）は https:// で始まるURLにしてください" ;;
esac
command -v systemctl >/dev/null 2>&1 || fail "systemdが必要です"
command -v curl >/dev/null 2>&1 || fail "curlが必要です"

# 署名の検証（ed25519 の -rawin）には、OpenSSL 3.0以降が要る。RHEL系 8 は、EPEL の openssl3 を使える
OPENSSL=""
for c in openssl openssl3; do
    if command -v "$c" >/dev/null 2>&1 && "$c" pkeyutl -help 2>&1 | grep -q -- '-rawin'; then
        OPENSSL="$c"
        break
    fi
done
[ -n "$OPENSSL" ] || fail "署名の検証に、OpenSSL 3.0以降が必要です（このサーバー: $(openssl version 2>/dev/null || echo 'openssl が無い')）。RHEL系 8 では、EPEL の openssl3 を入れてから、実行し直してください（sudo dnf install epel-release && sudo dnf install openssl3）。"

case "$(uname -s)" in Linux) ;; *) fail "Linuxだけに対応しています" ;; esac
case "$(uname -m)" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) fail "対応していないCPUです: $(uname -m)" ;;
esac

# 入れ直しのときは、今の設定のトークンを使い続けられる（トークンは一度しか表示されず、手元に残っていないため）。
# 送り先が同じときだけ使う（別のWebProxiedへ、このトークンを送らない）。トークンは表示しない
CURRENT_TOKEN=""
TOKEN_REUSED=0
if [ -r "$CONFIG_DIR/agent.conf" ]; then
    CURRENT_API_URL="$(sed -n 's/^API_URL=//p' "$CONFIG_DIR/agent.conf" | tail -n 1 | tr -d " \"'\r")"
    if [ "${CURRENT_API_URL%/}" = "${API_URL%/}" ]; then
        CURRENT_TOKEN="$(sed -n 's/^TOKEN=//p' "$CONFIG_DIR/agent.conf" | tail -n 1 | tr -d " \"'\r")"
        # 正しい形（wpa_ と英数字）でなければ使わない
        case "$CURRENT_TOKEN" in
            wpa_*) case "${CURRENT_TOKEN#wpa_}" in ""|*[!A-Za-z0-9]*) CURRENT_TOKEN="" ;; esac ;;
            *) CURRENT_TOKEN="" ;;
        esac
    fi
fi

if [ -z "$TOKEN" ]; then
    # 端末の無い環境（cloud-init・SSM Run Command・Ansible・ptyの無いsudo）では、/dev/tty が読めても書き込めない
    if ! [ -r /dev/tty ] || ! ( : > /dev/tty ) 2>/dev/null; then
        [ -n "$CURRENT_TOKEN" ] || fail "端末が無いため、トークンを入力できません（--token-file <ファイル> でトークンを渡してください）"
        echo "端末が無いため、今の設定のトークンを使い続けます（新しいトークンにするときは、--token-file <ファイル> で渡してください）"
        TOKEN="$CURRENT_TOKEN"
        TOKEN_REUSED=1
    else
        if [ -n "$CURRENT_TOKEN" ]; then
            printf 'WebProxiedのサーバーの画面に表示されたトークンを貼り付けて、Enterを押してください（今のトークンを使い続けるときは、何も入れずにEnter）: ' > /dev/tty
        else
            printf 'WebProxiedのサーバーの画面に表示されたトークンを貼り付けて、Enterを押してください: ' > /dev/tty
        fi
        stty -echo < /dev/tty 2>/dev/null || true
        IFS= read -r TOKEN < /dev/tty || true
        stty echo < /dev/tty 2>/dev/null || true
        echo > /dev/tty
        TOKEN="$(printf '%s' "$TOKEN" | tr -d ' \r\n')"
        if [ -z "$TOKEN" ] && [ -n "$CURRENT_TOKEN" ]; then
            echo "今の設定のトークンを使い続けます"
            TOKEN="$CURRENT_TOKEN"
            TOKEN_REUSED=1
        fi
    fi
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

printf '%s\n' "$PUBLIC_KEY_PEM" > "$WORK/release.pub.pem"
if ! "$OPENSSL" pkeyutl -verify -pubin -inkey "$WORK/release.pub.pem" -rawin -in "$WORK/checksums.txt" -sigfile "$WORK/checksums.txt.sig" >/dev/null 2>&1; then
    fail "署名を検証できませんでした。配布物が改ざんされている可能性があるため、導入を中止しました。WebProxiedのサポートへお問い合わせください。"
fi
echo "署名を検証しました"

EXPECTED="$(grep " ${ASSET}\$" "$WORK/checksums.txt" | awk '{print $1}')"
ACTUAL="$(sha256sum "$WORK/$ASSET" | awk '{print $1}')"
[ -n "$EXPECTED" ] && [ "$EXPECTED" = "$ACTUAL" ] || fail "SHA-256が一致しません"

# 専用のユーザーとグループ
getent group webproxied-agent >/dev/null 2>&1 || groupadd --system webproxied-agent 2>/dev/null || groupadd -r webproxied-agent
if ! id webproxied-agent >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir "$PREFIX" --shell /usr/sbin/nologin -g webproxied-agent webproxied-agent 2>/dev/null \
        || useradd -r -M -d "$PREFIX" -s /sbin/nologin -g webproxied-agent webproxied-agent
fi

# アクセスログの読み取り（足したディレクトリは、agent.conf の LOG_ACL_DIRS に記録する）
read_logs

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
# アクセスログを読めるよう、このスクリプトが読み取りの ACL を足したディレクトリ（入れ直し・削除のときに、これだけを外す）
LOG_ACL_DIRS=${LOG_ACL_DIRS}
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
# /home の下のアプリ（KUSANAGI など）の版と、/home の容量を読めるよう、読み取りだけで見せる（読めるのは、専用ユーザーの権限の範囲だけ）
ProtectHome=read-only
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
SEND_ERROR="WebProxiedへ送れませんでした。トークンとネットワーク（${API_URL} への接続）を確認してください"
if [ "$TOKEN_REUSED" = "1" ]; then
    # 画面でトークンを作り直していると、今の設定のトークンは無効になっている
    SEND_ERROR="${SEND_ERROR}（今の設定のトークンを使い続けました。画面でトークンを作り直した場合は、新しいトークンを貼り付けて、実行し直してください）"
fi
if command -v runuser >/dev/null 2>&1; then
    runuser -u webproxied-agent -- "$PREFIX/webproxied-agent" once || fail "$SEND_ERROR"
else
    su -s /bin/sh webproxied-agent -c "$PREFIX/webproxied-agent once" || fail "$SEND_ERROR"
fi

# 入れ直し（トークンの作り直し・版の更新）でも、新しい設定と実行ファイルで動くよう、再起動する
systemctl enable webproxied-agent >/dev/null 2>&1
systemctl restart webproxied-agent
echo "導入しました。WebProxiedのサーバーの画面で、接続できたことを確認してください。"
echo "$LOG_SUMMARY"

sshd_hint
}

# has_acl は、ディレクトリに、エージェントのユーザーの ACL（ふつう・デフォルト）があるか。
has_acl() {
    getfacl -p --omit-header "$1" 2>/dev/null | grep -q "^\(default:\)\{0,1\}user:${AGENT_USER}:"
}

# remove_acl は、ディレクトリ（と中のファイル）から、エージェントのユーザーの ACL を外す。無くても止まらない。
# シンボリックリンクは辿らない（-P）
remove_acl() {
    [ -d "$1" ] || return 0
    command -v setfacl >/dev/null 2>&1 || return 0
    setfacl -R -P -x "u:${AGENT_USER}" "$1" 2>/dev/null || true
    setfacl -x "d:u:${AGENT_USER}" "$1" 2>/dev/null || true
}

# add_acl は、ディレクトリに読み取りと通過、中のファイルに読み取り、新しいファイルに引き継ぐデフォルトACL を足す。
# ファイルの読み取りは、ファイルの権限（マスク）を超えない。シンボリックリンクは辿らない（-P）
add_acl() {
    setfacl -m "u:${AGENT_USER}:rx" "$1" \
        && setfacl -R -P -m "u:${AGENT_USER}:rX" "$1" \
        && setfacl -d -m "u:${AGENT_USER}:r" "$1"
}

# in_list は、空白で区切った一覧に、その語があるか。
in_list() {
    case " $2 " in *" $1 "*) return 0 ;; esac
    return 1
}

# read_logs は、アクセスログを読めるよう、Webサーバーのログのディレクトリに読み取りの ACL を足す。
# 前にこのスクリプトが足したもの（LOG_ACL_BEFORE）でない、管理者が足した ACL は、触らない。
# 結果を LOG_ACL_DIRS・LOG_SUMMARY に入れる。
read_logs() {
    LOG_ACL_DIRS=""
    LOG_SUMMARY=""
    NO_ACL=0
    MANUAL=""

    for d in $LOG_DIRS; do
        # シンボリックリンクのディレクトリは、リンクの先を変えないよう、触らない
        [ -d "$d" ] && ! [ -L "$d" ] || continue
        if ! command -v setfacl >/dev/null 2>&1 || ! command -v getfacl >/dev/null 2>&1; then
            NO_ACL=1
            break
        fi
        if has_acl "$d" && ! in_list "$d" "$LOG_ACL_BEFORE"; then
            # 管理者が足したもの
            MANUAL="${MANUAL:+$MANUAL }$d"
            continue
        fi
        if add_acl "$d" 2>/dev/null; then
            LOG_ACL_DIRS="${LOG_ACL_DIRS:+$LOG_ACL_DIRS }$d"
        else
            # ACL が使えないファイルシステムなど。足しかけたものを戻す
            remove_acl "$d"
            NO_ACL=1
        fi
    done

    # 前に足して、今回は足さないもの（ディレクトリが消えた・ACL を使えなくなった）を外す
    for d in $LOG_ACL_BEFORE; do
        in_list "$d" "$LOG_ACL_DIRS" || remove_acl "$d"
    done

    if [ -n "$LOG_ACL_DIRS" ]; then
        LOG_SUMMARY="アクセスログ: ${LOG_ACL_DIRS} に、読み取りだけを足しました（ACL）"
    elif [ "$NO_ACL" = "0" ] && [ -z "$MANUAL" ]; then
        LOG_SUMMARY="アクセスログ: Webサーバーのログのディレクトリが無いため、何も足していません"
    fi
    if [ -n "$MANUAL" ]; then
        LOG_SUMMARY="${LOG_SUMMARY:+$LOG_SUMMARY
}アクセスログ: ${MANUAL} は、すでに ${AGENT_USER} の ACL があるため、変えていません"
    fi
    if [ "$NO_ACL" = "1" ]; then
        LOG_SUMMARY="${LOG_SUMMARY:+$LOG_SUMMARY
}アクセスログ: ACL を使えないため、一部またはすべてに読み取りを足していません。アクセスログの5xxの監視を使うには、acl のパッケージを入れてから、実行し直してください"
    fi
}

# dev_host は、URL の送り先が開発環境か（localhost・ループバック・プライベートIPv4・.localhost・.test・.internal）。
dev_host() {
    h="${1#*://}"
    h="${h%%/*}"
    h="${h%%\?*}"
    h="${h%%#*}"
    h="${h##*@}"
    case "$h" in
        \[*) h="${h#\[}"; h="${h%%]*}" ;;
        *) h="${h%%:*}" ;;
    esac
    h="$(printf '%s' "$h" | tr 'A-Z' 'a-z')"
    case "$h" in
        localhost|*.localhost|*.test|*.internal|::1) return 0 ;;
    esac
    # 数字と点だけ（IPv4）のときだけ、アドレスの範囲で判断する（10.example.com などを許さない）
    case "$h" in
        ""|*[!0-9.]*) return 1 ;;
    esac
    case "$h" in
        127.*|10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*) return 0 ;;
    esac
    return 1
}

# uninstall は、エージェントを削除する。ログのディレクトリからは、エージェントのユーザーの ACL を外す
# （ユーザーを消すと、残った ACL は意味を持たないため、記録に無いものも外す）。
uninstall() {
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable --now webproxied-agent >/dev/null 2>&1 || true
    fi
    for d in $LOG_DIRS $LOG_ACL_BEFORE; do
        remove_acl "$d"
    done
    rm -rf "$PREFIX" "$CONFIG_DIR" /etc/systemd/system/webproxied-agent.service
    if command -v systemctl >/dev/null 2>&1; then
        systemctl daemon-reload || true
    fi
    if id "$AGENT_USER" >/dev/null 2>&1; then
        userdel "$AGENT_USER" 2>/dev/null || true
    fi
    getent group "$AGENT_USER" >/dev/null 2>&1 && { groupdel "$AGENT_USER" 2>/dev/null || true; }
    echo "削除しました（サービス・${PREFIX}・${CONFIG_DIR}・ユーザー ${AGENT_USER}・ログのディレクトリの読み取り）"
}

# sshd の設定を、エージェントのユーザーが読めないとき（RHEL系の既定の権限）に、読めるようにする手順を案内する。
# 権限は変えない（変えるかは、サーバーの管理者が決める）
sshd_hint() {
    SSHD_CONFIG="/etc/ssh/sshd_config"
    [ -f "$SSHD_CONFIG" ] || return 0
    command -v runuser >/dev/null 2>&1 || return 0

    # 読むファイルを root で並べてから、エージェントのユーザーで読めるかを確かめる（ディレクトリは、一覧と通過も要る）
    set -- "$SSHD_CONFIG"
    HAS_DIR=0
    HAS_CONF=0
    if [ -d "$SSHD_CONFIG.d" ]; then
        HAS_DIR=1
        set -- "$@" "$SSHD_CONFIG.d"
        for f in "$SSHD_CONFIG.d"/*.conf; do
            [ -f "$f" ] || continue
            HAS_CONF=1
            set -- "$@" "$f"
        done
    fi
    if runuser -u webproxied-agent -- /bin/sh -c '
        for f; do
            [ -r "$f" ] || exit 1
            if [ -d "$f" ]; then [ -x "$f" ] || exit 1; fi
        done' sh "$@" 2>/dev/null; then
        return 0
    fi

    echo
    echo "注意: webproxied-agent のユーザーが、sshd の設定（${SSHD_CONFIG} など）を読めないため、WebProxiedの画面で、SSHの設定は「確認できず」になります。"
    echo "読めるようにするには（任意）、次を実行してください。読み取りだけを足します（書き込みや、ほかのファイルの読み取りは増えません）。"
    echo "  sudo setfacl -m u:webproxied-agent:r ${SSHD_CONFIG}"
    if [ "$HAS_DIR" = "1" ]; then
        echo "  sudo setfacl -m u:webproxied-agent:rx ${SSHD_CONFIG}.d"
    fi
    if [ "$HAS_CONF" = "1" ]; then
        echo "  sudo setfacl -m u:webproxied-agent:r ${SSHD_CONFIG}.d/*.conf"
    fi
    echo "  sudo systemctl restart webproxied-agent    # すぐに画面へ反映する"
    if ! command -v setfacl >/dev/null 2>&1; then
        echo "  （setfacl が無いときは、先に acl のパッケージを入れてください）"
    fi
    echo "openssh-server を更新すると、足した読み取りは消えます。更新の後と、${SSHD_CONFIG}.d にファイルを足した後は、もう一度実行してください。"
}

main "$@"
