#!/usr/bin/env bash
# Installs the e2e tools at pinned versions into a directory (default
# ./bin/tools) and checks their checksums. Add the directory to PATH.
set -euo pipefail

DEST=${1:-bin/tools}
KIND_VERSION=v0.33.0
KUBECTL_VERSION=v1.37.1
FLUX_VERSION=2.10.0
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64 | arm64) ARCH=arm64 ;; esac

mkdir -p "$DEST"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSLo "$tmp/kind" "https://kind.sigs.k8s.io/dl/$KIND_VERSION/kind-$OS-$ARCH"
echo "$(curl -fsSL "https://kind.sigs.k8s.io/dl/$KIND_VERSION/kind-$OS-$ARCH.sha256sum" | awk '{print $1}')  $tmp/kind" | sha256sum -c -

curl -fsSLo "$tmp/kubectl" "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/$OS/$ARCH/kubectl"
echo "$(curl -fsSL "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/$OS/$ARCH/kubectl.sha256")  $tmp/kubectl" | sha256sum -c -

archive=flux_${FLUX_VERSION}_${OS}_${ARCH}.tar.gz
curl -fsSLo "$tmp/$archive" "https://github.com/fluxcd/flux2/releases/download/v$FLUX_VERSION/$archive"
(cd "$tmp" && curl -fsSL "https://github.com/fluxcd/flux2/releases/download/v$FLUX_VERSION/flux_${FLUX_VERSION}_checksums.txt" | grep " $archive\$" | sha256sum -c -)
tar -C "$tmp" -xzf "$tmp/$archive" flux

install -m 0755 "$tmp/kind" "$tmp/kubectl" "$tmp/flux" "$DEST/"
echo "installed kind, kubectl and flux into $DEST"
