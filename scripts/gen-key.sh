#!/bin/sh
# 自動更新の署名に使う鍵（ed25519）を作る。秘密鍵は、GitHubとは別の場所（手元のPCなど）で保管し、共有しない。
#   sh scripts/gen-key.sh ~/secure/webproxied-agent-release.key
set -eu
KEY="${1:?秘密鍵の保存先を指定してください}"
[ ! -e "$KEY" ] || { echo "既にあります: $KEY" >&2; exit 1; }

umask 077
openssl genpkey -algorithm ed25519 -out "$KEY"

echo "秘密鍵: $KEY（共有しない・GitHubに置かない）"
echo
echo "internal/update/keys/release.pub に、次の1行を加えてください:"
openssl pkey -in "$KEY" -pubout -outform DER | tail -c 32 | base64
echo
echo "packaging/install.sh の PUBLIC_KEY_PEM に、次の内容を入れてください:"
openssl pkey -in "$KEY" -pubout
