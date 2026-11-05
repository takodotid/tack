#!/usr/bin/env bash
# Installs tools into DIR, each at the version and SHA-256 pinned here ([RFC#0009] Requirement 25), and fails on a
# mismatch. Without a tool named, it installs the end-to-end test's tools: kind, kubectl, flux and helm. Linux only,
# on amd64 or arm64. Add DIR to PATH.
#
#   hack/tools/install.sh [DIR [TOOL...]]   DIR defaults to bin/tools; TOOL is kind, kubectl, flux, helm or cosign
set -euo pipefail

DEST=${1:-bin/tools}
shift || true
tools=("$@")
[[ ${#tools[@]} -gt 0 ]] || tools=(kind kubectl flux helm)

KIND_VERSION=v0.33.0
KUBECTL_VERSION=v1.37.1
FLUX_VERSION=2.10.0
HELM_VERSION=v4.3.1
COSIGN_VERSION=v3.1.4

# The SHA-256 of each download, by tool and architecture, copied from each project's release.
declare -A SHA256=(
  [kind-amd64]=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d
  [kind-arm64]=20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2
  [kubectl-amd64]=65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8
  [kubectl-arm64]=ff749f4b78d9c4f1ec87307df9b50119ed819e2094aa9810cb9acffc3286c8c7
  [flux-amd64]=208dd7012c04fedeed5b2d0b6e63df3b15b1c34a381527811685c098bc9f8659
  [flux-arm64]=c3c435052e5e18156858bc73f26bcadb62821a6e30951b4b302beb4e65a532c2
  [helm-amd64]=0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b
  [helm-arm64]=8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88
  [cosign-amd64]=424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa
  [cosign-arm64]=ade83b7aac62e131cacfe96d958948b2150cc9efec46a570a2649b8e27dedad2
)

[[ $(uname -s) == Linux ]] || { echo "install.sh: the tools are pinned for Linux only" >&2; exit 1; }
case $(uname -m) in
x86_64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) echo "install.sh: the tools are pinned for amd64 and arm64 only, not $(uname -m)" >&2; exit 1 ;;
esac

mkdir -p "$DEST"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# fetch TOOL URL FILE downloads URL into $tmp/FILE, and fails unless its SHA-256 is the one pinned for TOOL.
fetch() {
  local tool=$1 url=$2 file=$3
  curl -fsSLo "$tmp/$file" "$url"
  echo "${SHA256[$tool-$ARCH]}  $tmp/$file" | sha256sum -c --quiet - || { echo "install.sh: $url does not match its pinned SHA-256" >&2; exit 1; }
}

for tool in "${tools[@]}"; do
  case $tool in
  kind) fetch kind "https://kind.sigs.k8s.io/dl/$KIND_VERSION/kind-linux-$ARCH" kind ;;
  kubectl) fetch kubectl "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$ARCH/kubectl" kubectl ;;
  flux)
    fetch flux "https://github.com/fluxcd/flux2/releases/download/v$FLUX_VERSION/flux_${FLUX_VERSION}_linux_$ARCH.tar.gz" flux.tar.gz
    tar -C "$tmp" -xzf "$tmp/flux.tar.gz" flux
    ;;
  helm)
    fetch helm "https://get.helm.sh/helm-$HELM_VERSION-linux-$ARCH.tar.gz" helm.tar.gz
    tar -C "$tmp" -xzf "$tmp/helm.tar.gz" --strip-components 1 "linux-$ARCH/helm"
    ;;
  cosign) fetch cosign "https://github.com/sigstore/cosign/releases/download/$COSIGN_VERSION/cosign-linux-$ARCH" cosign ;;
  *) echo "install.sh: unknown tool $tool" >&2; exit 1 ;;
  esac
  install -m 0755 "$tmp/$tool" "$DEST/"
done
echo "installed ${tools[*]} into $DEST"
