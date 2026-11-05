#!/bin/sh
# リリース（下書き）の checksums.txt に署名し、署名を添付して公開する。秘密鍵は手元で使い、GitHubへは送らない。
#   sh scripts/sign-release.sh v1.2.3 ~/secure/webproxied-agent-release.key
# OpenSSL 3.0以降と、GitHub CLI（gh）が必要。
set -eu
TAG="${1:?リリースのタグを指定してください（例: v1.2.3）}"
KEY="${2:?秘密鍵を指定してください}"
REPO="cloud-guild/webproxied-agent"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

gh release download "$TAG" --repo "$REPO" --pattern checksums.txt --dir "$WORK"
echo "署名する内容:"
cat "$WORK/checksums.txt"

openssl pkeyutl -sign -inkey "$KEY" -rawin -in "$WORK/checksums.txt" -out "$WORK/checksums.txt.sig"
openssl pkey -in "$KEY" -pubout > "$WORK/pub.pem"
openssl pkeyutl -verify -pubin -inkey "$WORK/pub.pem" -rawin -in "$WORK/checksums.txt" -sigfile "$WORK/checksums.txt.sig"

gh release upload "$TAG" "$WORK/checksums.txt.sig" --repo "$REPO" --clobber
gh release edit "$TAG" --repo "$REPO" --draft=false
echo "署名を添付して、公開しました: $TAG"
