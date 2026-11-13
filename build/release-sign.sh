#!/bin/sh
# Signs and publishes a release that the Release workflow left as a draft:
#
#   build/release-sign.sh v1.31.0
#
# The release key's private half is kept apart from GitHub, in
# ~/.config/switcher-release/release-1 (SWITCHER_RELEASE_KEY names another),
# so whoever can change a release on GitHub cannot sign one. Before it signs,
# this checks that the draft is the tag's on main, that every file in it is
# in SHA256SUMS with the checksum it has, that nothing else is listed, and
# that the Linux build runs as the version it says. It then signs
# SHA256SUMS (ssh-keygen -Y sign -n switcher-release), checks the signature
# against the keys install.sh trusts, uploads SHA256SUMS.sig and publishes.
set -eu

repo=00xmario/Switcher
key=${SWITCHER_RELEASE_KEY:-$HOME/.config/switcher-release/release-1}

fail() {
	echo "release-sign: $*" >&2
	exit 1
}

[ $# = 1 ] || fail "usage: build/release-sign.sh vX.Y.Z"
tag=$1
case "$tag" in v[0-9]*.[0-9]*.[0-9]*) ;; *) fail "unexpected tag $tag" ;; esac
case "${tag#v}" in *[!0-9.]*) fail "unexpected tag $tag" ;; esac
version=${tag#v}
[ -r "$key" ] || fail "no release key at $key"
cd "$(dirname "$0")/.."
root=$(pwd)

# The tag is on main, here and on GitHub.
git fetch --quiet origin main
git fetch --quiet --force origin "refs/tags/$tag:refs/tags/$tag"
commit=$(git rev-parse "$tag^{commit}")
git merge-base --is-ancestor "$commit" origin/main || fail "$tag ($commit) is not on main"
remote=$(git ls-remote origin "refs/tags/$tag^{}" | cut -f1)
[ -n "$remote" ] || remote=$(git ls-remote origin "refs/tags/$tag" | cut -f1)
[ "$remote" = "$commit" ] || fail "GitHub's $tag is $remote, not $commit"

# A draft, not signed yet.
[ "$(gh release view "$tag" --repo "$repo" --json isDraft --jq .isDraft)" = true ] ||
	fail "$tag is not a draft: published already, or not made by the Release workflow"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gh release download "$tag" --repo "$repo" --dir "$work"
cd "$work"
[ ! -e SHA256SUMS.sig ] || fail "$tag already carries a signature"
[ -s SHA256SUMS ] || fail "$tag has no SHA256SUMS"

# Every file is listed, with its checksum; nothing else is.
listed=$(awk '{ name = $2; sub(/^\*/, "", name); print name }' SHA256SUMS | sort)
present=$(ls | grep -v '\.sha256$' | grep -vx SHA256SUMS | sort)
[ "$listed" = "$present" ] || fail "SHA256SUMS does not list exactly the release's files"
[ "$(printf '%s\n' "$listed" | uniq -d)" = "" ] || fail "SHA256SUMS lists a file twice"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum --quiet -c SHA256SUMS || fail "a file does not match SHA256SUMS"
else
	shasum -a 256 -c SHA256SUMS >/dev/null || fail "a file does not match SHA256SUMS"
fi
for name in Switcher_$version.dmg \
	switcher-server_${version}_darwin_arm64 switcher-server_${version}_darwin_amd64 \
	switcher-server_${version}_linux_amd64 switcher-server_${version}_linux_arm64 \
	switcher-tailnet_${version}_darwin_arm64 switcher-tailnet_${version}_darwin_amd64 \
	switcher-tailnet_${version}_linux_amd64 switcher-tailnet_${version}_linux_arm64; do
	printf '%s\n' "$listed" | grep -qx "$name" || fail "$tag has no $name"
done

# The build for this computer runs as the version it is.
case "$(uname -s)/$(uname -m)" in
Linux/x86_64) own=switcher-server_${version}_linux_amd64 ;;
Linux/aarch64) own=switcher-server_${version}_linux_arm64 ;;
Darwin/arm64) own=switcher-server_${version}_darwin_arm64 ;;
Darwin/x86_64) own=switcher-server_${version}_darwin_amd64 ;;
*) own= ;;
esac
if [ -n "$own" ]; then
	chmod 0700 "$own"
	[ "$("./$own" version)" = "switcher $version" ] || fail "$own does not say it is $version"
fi

ssh-keygen -q -Y sign -f "$key" -n switcher-release SHA256SUMS </dev/null
# The signature checks out against the keys the installer trusts.
sed -n "/^release_signers='/,/'\$/p" "$root/install.sh" | sed "s/^release_signers='//; s/'\$//" >allowed_signers
ssh-keygen -Y verify -f allowed_signers -I switcher-release -n switcher-release -s SHA256SUMS.sig <SHA256SUMS >/dev/null ||
	fail "the signature does not check out against install.sh's keys"

gh release upload "$tag" SHA256SUMS.sig --repo "$repo"
gh release edit "$tag" --repo "$repo" --draft=false --latest >/dev/null
echo "Signed and published $tag."
