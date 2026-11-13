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

# One line per tool and architecture: the tool, its version, the architecture, and the SHA-256 of that download, copied
# from the project's release. Both lines of a tool must name the same version. renovate.json asks Renovate to update
# each line's version, and for kind, flux and cosign its SHA-256 too ([RFC#0009] Limitations).
PINS='
kind     v0.33.0  amd64  aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d
kind     v0.33.0  arm64  20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2
kubectl  v1.37.2  amd64  c04487523e7aa7d1493f7808174c899db93d7a9cf57531420cd76b9bd415f8f4
kubectl  v1.37.2  arm64  fcc16f63186659d9722014b7e02d5fdd2d4e669cb8c0ab66365d7d3b8ba03509
flux     v2.10.0  amd64  208dd7012c04fedeed5b2d0b6e63df3b15b1c34a381527811685c098bc9f8659
flux     v2.10.0  arm64  c3c435052e5e18156858bc73f26bcadb62821a6e30951b4b302beb4e65a532c2
helm     v4.3.2   amd64  d1c0278d46beab79582d7e50f5b3dc206944ba8e577cde618513fd07daa53983
helm     v4.3.2   arm64  df31cb61680ef728f82c770e5280dd565617bee6b039d37e85c677288be4bbb0
cosign   v3.1.4   amd64  424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa
cosign   v3.1.4   arm64  ade83b7aac62e131cacfe96d958948b2150cc9efec46a570a2649b8e27dedad2
'

[[ $(uname -s) == Linux ]] || { echo "install.sh: the tools are pinned for Linux only" >&2; exit 1; }
# TACK_TOOLS_ARCH checks the downloads of another architecture, such as the arm64 pins on an amd64 runner.
case ${TACK_TOOLS_ARCH:-$(uname -m)} in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) echo "install.sh: the tools are pinned for amd64 and arm64 only, not ${TACK_TOOLS_ARCH:-$(uname -m)}" >&2; exit 1 ;;
esac

mkdir -p "$DEST"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# fetch URL FILE downloads URL into $tmp/FILE, and fails unless its SHA-256 is $sha, the one pinned for the tool.
fetch() {
  local url=$1 file=$2
  curl -fsSLo "$tmp/$file" "$url"
  echo "$sha  $tmp/$file" | sha256sum -c --quiet - || { echo "install.sh: $url does not match its pinned SHA-256" >&2; exit 1; }
}

for tool in "${tools[@]}"; do
  case $tool in
  kind | kubectl | flux | helm | cosign) ;;
  *) echo "install.sh: unknown tool $tool" >&2; exit 1 ;;
  esac
  version=$(awk -v t="$tool" '$1 == t { print $2 }' <<<"$PINS" | sort -u)
  [[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "install.sh: the lines of $tool pin versions '${version//$'\n'/ }'; both must pin one version vX.Y.Z" >&2; exit 1; }
  sha=$(awk -v t="$tool" -v a="$ARCH" '$1 == t && $3 == a { print $4 }' <<<"$PINS")
  [[ $sha =~ ^[0-9a-f]{64}$ ]] || { echo "install.sh: no SHA-256 pinned for $tool on $ARCH" >&2; exit 1; }
  case $tool in
  kind) fetch "https://kind.sigs.k8s.io/dl/$version/kind-linux-$ARCH" kind ;;
  kubectl) fetch "https://dl.k8s.io/release/$version/bin/linux/$ARCH/kubectl" kubectl ;;
  flux)
    fetch "https://github.com/fluxcd/flux2/releases/download/$version/flux_${version#v}_linux_$ARCH.tar.gz" flux.tar.gz
    tar -C "$tmp" -xzf "$tmp/flux.tar.gz" flux
    ;;
  helm)
    fetch "https://get.helm.sh/helm-$version-linux-$ARCH.tar.gz" helm.tar.gz
    tar -C "$tmp" -xzf "$tmp/helm.tar.gz" --strip-components 1 "linux-$ARCH/helm"
    ;;
  cosign) fetch "https://github.com/sigstore/cosign/releases/download/$version/cosign-linux-$ARCH" cosign ;;
  esac
  install -m 0755 "$tmp/$tool" "$DEST/"
done
echo "installed ${tools[*]} into $DEST"
