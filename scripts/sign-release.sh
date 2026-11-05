#!/bin/sh
# リリース（下書き）を手元で作り直してハッシュを照らし合わせ、一致したときだけ checksums.txt に署名し、公開する。
# 秘密鍵は手元で使い、GitHubへは送らない。ビルドの環境（GitHub Actions）が乗っ取られても、作り直した結果と違えば署名しない。
#   sh scripts/sign-release.sh v1.2.3 ~/secure/webproxied-agent-release.key
# OpenSSL 3.0以降・GitHub CLI（gh）・Docker（ビルドの環境をそろえる）が必要。
set -eu
TAG="${1:?リリースのタグを指定してください（例: v1.2.3）}"
KEY="${2:?秘密鍵を指定してください}"
REPO="cloud-guild/webproxied-agent"
GO_IMAGE="golang:1.24.13-bookworm"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

gh release download "$TAG" --repo "$REPO" --pattern checksums.txt --dir "$WORK"

# タグのコードを取り出し、リリースと同じ方法で作り直す
git clone --quiet --depth 1 --branch "$TAG" "https://github.com/${REPO}.git" "$WORK/src"
docker run --rm -v "$WORK/src":/src -w /src -e CGO_ENABLED=0 "$GO_IMAGE" sh -c "
  for arch in amd64 arm64; do
    GOOS=linux GOARCH=\$arch go build -trimpath -buildvcs=false -ldflags '-s -w -X main.version=${TAG}' -o dist/webproxied-agent_linux_\$arch ./cmd/webproxied-agent
  done"
(cd "$WORK/src/dist" && { echo "version: ${TAG}"; sha256sum webproxied-agent_linux_*; }) > "$WORK/rebuilt.txt"

if ! diff -u "$WORK/rebuilt.txt" "$WORK/checksums.txt"; then
    echo "エラー: 手元で作り直した結果と、リリースのchecksums.txtが一致しません。署名しません。" >&2
    exit 1
fi

echo "作り直した結果と一致しました。署名します:"
cat "$WORK/checksums.txt"

openssl pkeyutl -sign -inkey "$KEY" -rawin -in "$WORK/checksums.txt" -out "$WORK/checksums.txt.sig"
openssl pkey -in "$KEY" -pubout > "$WORK/pub.pem"
openssl pkeyutl -verify -pubin -inkey "$WORK/pub.pem" -rawin -in "$WORK/checksums.txt" -sigfile "$WORK/checksums.txt.sig"

gh release upload "$TAG" "$WORK/checksums.txt.sig" --repo "$REPO" --clobber
gh release edit "$TAG" --repo "$REPO" --draft=false
echo "署名を添付して、公開しました: $TAG"
