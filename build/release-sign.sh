#!/bin/sh
# Signs and publishes a release that the Release workflow left as a draft:
#
#   build/release-sign.sh v1.31.0 <commit>
#
# <commit> is the commit you tagged, as you merged it: the release must be
# built from it. The release key's private half is kept apart from GitHub,
# in ~/.config/switcher-release/release-1 (SWITCHER_RELEASE_KEY names
# another), so whoever can change a release on GitHub cannot sign one; this
# script is what makes sure it signs only what was built from <commit>:
#
# - the tag is <commit>, here and on GitHub, and <commit> is on main;
# - exactly one release carries the tag, a draft, with exactly the files the
#   workflow makes;
# - the files that build the same everywhere (the Linux builds and the
#   Tailscale add-ons) are built again here from <commit> and must be
#   identical;
# - every file carries GitHub's attestation that the Release workflow built
#   it from <commit> for this tag, on GitHub's own runners;
# - the list of checksums is written here, from the checked files, and the
#   per-file checksums older releases read must agree with it.
#
# Nothing downloaded is ever run. Then it signs the list (ssh-keygen -Y sign
# -n switcher-release), checks the signature against the keys install.sh
# trusts, uploads the list and its signature, and publishes.
set -eu

repo=00xmario/Switcher
workflow=$repo/.github/workflows/release.yml
key=${SWITCHER_RELEASE_KEY:-$HOME/.config/switcher-release/release-1}

fail() {
	echo "release-sign: $*" >&2
	exit 1
}

[ $# = 2 ] || fail "usage: build/release-sign.sh vX.Y.Z <commit>"
tag=$1 commit=$2
case "$tag" in v[0-9]*.[0-9]*.[0-9]*) ;; *) fail "unexpected tag $tag" ;; esac
case "${tag#v}" in *[!0-9.]*) fail "unexpected tag $tag" ;; esac
case "$commit" in *[!0-9a-f]*) fail "the commit must be its full id" ;; esac
[ ${#commit} = 40 ] || fail "the commit must be its full id"
version=${tag#v}
case "$key" in /*) ;; *) key=$(pwd)/$key ;; esac
[ -r "$key" ] || fail "no release key at $key"
cd "$(dirname "$0")/.."
root=$(pwd)

# The tag is the commit, here and on GitHub, and the commit is on main.
git fetch --quiet origin main
git fetch --quiet --force origin "refs/tags/$tag:refs/tags/$tag"
[ "$(git rev-parse "$tag^{commit}")" = "$commit" ] || fail "$tag is not $commit"
git merge-base --is-ancestor "$commit" origin/main || fail "$commit is not on main"
remote=$(git ls-remote origin "refs/tags/$tag^{}" | cut -f1)
[ -n "$remote" ] || remote=$(git ls-remote origin "refs/tags/$tag" | cut -f1)
[ "$remote" = "$commit" ] || fail "GitHub's $tag is $remote, not $commit"

# One release carries the tag, and it is a draft.
releases=$(gh api "repos/$repo/releases" --paginate --jq "[.[] | select(.tag_name == \"$tag\")] | length" | awk '{ n += $1 } END { print n + 0 }')
[ "$releases" = 1 ] || fail "$releases releases carry $tag; there must be exactly one"
[ "$(gh release view "$tag" --repo "$repo" --json isDraft --jq .isDraft)" = true ] ||
	fail "$tag is not a draft: published already, or not made by the Release workflow"

work=$(mktemp -d)
trap 'git -C "$root" worktree remove --force "$work/src" 2>/dev/null || true; rm -rf "$work"' EXIT
mkdir "$work/dl" "$work/built"
gh release download "$tag" --repo "$repo" --dir "$work/dl"
chmod 0600 "$work"/dl/*

# Exactly the files the workflow makes; a signature or list of an earlier
# attempt is replaced below.
artifacts="Switcher_$version.dmg"
for target in darwin_arm64 darwin_amd64 linux_amd64 linux_arm64; do
	artifacts="$artifacts switcher-server_${version}_$target switcher-tailnet_${version}_$target"
done
expected=$( (for f in $artifacts; do echo "$f"; echo "$f.sha256"; done; echo SHA256SUMS) | sort)
present=$(ls "$work/dl" | grep -vx SHA256SUMS.sig | sort)
[ "$present" = "$expected" ] || fail "$tag does not hold exactly the files the workflow makes"

# The files that build the same everywhere, built again here from the
# commit with the toolchain go.mod names, must be identical.
git worktree add --quiet --detach "$work/src" "$commit"
want=$(awk '$1 == "go" { print "go" $2 }' "$work/src/go.mod")
[ "$(cd "$work/src" && go env GOVERSION)" = "$want" ] || fail "go is $(go env GOVERSION), $tag is built with $want"
(
	cd "$work/src/cmd/switcher-tailnet"
	for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
		os=${target%/*} arch=${target#*/}
		CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$work/built/switcher-tailnet_${version}_${os}_$arch" .
	done
	cd "$work/src"
	for arch in amd64 arm64; do
		addon=$(sha256sum "$work/built/switcher-tailnet_${version}_linux_$arch" | cut -d' ' -f1)
		CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
			-ldflags "-s -w -X main.version=$version -X switcher/internal/remote.addonSHA256=$addon" \
			-o "$work/built/switcher-server_${version}_linux_$arch" .
	done
)
for f in "$work"/built/*; do
	cmp -s "$f" "$work/dl/$(basename "$f")" || fail "$(basename "$f") is not what $commit builds"
done

# Every file carries GitHub's attestation that the Release workflow built it
# from the commit, for this tag, on GitHub's runners.
for f in $artifacts; do
	gh attestation verify "$work/dl/$f" --repo "$repo" --signer-workflow "$workflow" \
		--source-ref "refs/tags/$tag" --source-digest "$commit" --deny-self-hosted-runners >/dev/null ||
		fail "$f carries no attestation that the Release workflow built it from $commit"
done

# The list is written here from the checked files; the per-file checksums
# older releases read must agree with it.
cd "$work/dl"
for f in $artifacts; do
	sum=$(sha256sum "$f" | cut -d' ' -f1)
	printf '%s  %s\n' "$sum" "$f"
	[ "$(awk '{ print $1 }' "$f.sha256")" = "$sum" ] || fail "$f.sha256 does not match $f"
done >"$work/SHA256SUMS"
cp "$work/SHA256SUMS" SHA256SUMS
rm -f SHA256SUMS.sig

ssh-keygen -q -Y sign -f "$key" -n switcher-release SHA256SUMS </dev/null
# The signature checks out against the keys the installer trusts.
sed -n "/^release_signers='/,/'\$/p" "$root/install.sh" | sed "s/^release_signers='//; s/'\$//" >"$work/allowed_signers"
ssh-keygen -Y verify -f "$work/allowed_signers" -I switcher-release -n switcher-release -s SHA256SUMS.sig <SHA256SUMS >/dev/null ||
	fail "the signature does not check out against install.sh's keys"

gh release upload "$tag" SHA256SUMS SHA256SUMS.sig --clobber --repo "$repo"
gh release edit "$tag" --repo "$repo" --draft=false --latest >/dev/null
echo "Signed and published $tag."
