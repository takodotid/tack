#!/usr/bin/env bash
# Builds every asset of a release at VERSION into dist/, and SHA256SUMS over them ([RFC#0009] Requirements 20 and 24):
# the archives of make dist, an SBOM per archive from make sbom, the CRD, and the chart, linted and packaged at X.Y.Z.
# Every file's time is SOURCE_DATE_EPOCH, the commit's time by default, so the same commit gives the same archives and
# chart. Fails unless dist/ then holds each asset once. Needs go, make, helm and GNU touch.
#
#   build.sh vX.Y.Z[-PRERELEASE]
set -euo pipefail

refuse() {
	echo "build.sh: $*" >&2
	exit 1
}

[[ $# -eq 1 ]] || refuse "usage: build.sh vX.Y.Z[-PRERELEASE]"
version=$1
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || refuse "version $version is not vX.Y.Z or vX.Y.Z-PRERELEASE"
cd "$(dirname "$0")/../.."
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}

make dist VERSION="$version"
make sbom VERSION="$version"
cp config/crd/failover.tako.id_sitefailovers.yaml dist/
helm lint --strict deploy/helm/tack
# helm package keeps each file's time, which the checkout set.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -R deploy/helm/tack "$work/"
find "$work/tack" -exec touch -d "@$SOURCE_DATE_EPOCH" {} +
helm package "$work/tack" --version "${version#v}" --app-version "${version#v}" -d dist

cd dist
platforms=(linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64)
assets=()
for p in "${platforms[@]}"; do
	case $p in windows-*) ext=zip ;; *) ext=tar.gz ;; esac
	assets+=("tack-$version-$p.$ext" "tack-$version-$p.cdx.json")
done
assets+=("tack-${version#v}.tgz" failover.tako.id_sitefailovers.yaml)
for a in "${assets[@]}"; do
	[[ -s $a ]] || refuse "dist/$a is missing or empty"
done
# Anything else that looks like an asset would be left out of SHA256SUMS, and so of the release.
shopt -s nullglob
for f in *.tar.gz *.zip *.cdx.json *.tgz *.yaml; do
	printf '%s\n' "${assets[@]}" | grep -qxF -- "$f" || refuse "dist/$f is not an asset of $version"
done
sha256sum "${assets[@]}" >SHA256SUMS
cat SHA256SUMS
echo "build.sh: ${#assets[@]} assets of $version and SHA256SUMS in dist/"
