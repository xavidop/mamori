#!/usr/bin/env bash
# The Go module release end to end (make release-check). internal/tools/release
# cuts a release; this checks that the go command can use it from outside the
# repository:
#   1. the go command, building each module from its tag (GOPROXY=direct, git
#      reading this repository), gets the hashes the release serves: what
#      proxy.golang.org builds from the tags is what the go.sum files hold;
#   2. from a scratch module, go install builds the CLI and it runs;
#   3. a program that imports the core, a provider with an SDK of its own, a
#      provider built on providers/httpcore, the server and an x/ module
#      builds there after go get of each at <version>, with no replace.
#
# Usage:
#   scripts/release-check.sh
#       Cuts a throwaway release of HEAD (RELEASE_CHECK_VERSION, default
#       v1.99.99-test) in a scratch clone and checks it. Nothing is tagged in
#       this repository.
#   scripts/release-check.sh --version vX.Y.Z --proxy DIR
#       Checks the release the tool cut in this repository, whose tags are
#       not pushed yet, with its file proxy in DIR.
#   scripts/release-check.sh --version vX.Y.Z --public
#       Checks a pushed release through proxy.golang.org and the checksum
#       database alone. It retries for up to 40 minutes while the proxy
#       fetches the new tags (RELEASE_CHECK_PUBLIC_WAIT, in seconds).
#
# Third-party modules come from the local module cache, then
# https://proxy.golang.org: no other network.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module=github.com/xavidop/mamori
version="${RELEASE_CHECK_VERSION:-v1.99.99-test}"
proxy=""
public=false
while [ $# -gt 0 ]; do
	case "$1" in
	--version) version="$2"; shift 2 ;;
	--proxy) proxy="$(cd "$2" && pwd)"; shift 2 ;;
	--public) public=true; shift ;;
	*) echo "usage: $0 [--version vX.Y.Z (--proxy DIR | --public)]" >&2; exit 2 ;;
	esac
done

fail() {
	echo "release-check: FAIL: $*" >&2
	exit 1
}

# retry runs a command, and with --public runs it again while proxy.golang.org
# has not fetched the new tags yet. The proxy remembers a version it did not
# find for up to 30 minutes, so the wait is 40 (RELEASE_CHECK_PUBLIC_WAIT, in
# seconds), counted from the first failure.
public_wait="${RELEASE_CHECK_PUBLIC_WAIT:-2400}"
retry() {
	local start=$SECONDS
	until "$@"; do
		if [ "$public" != true ] || [ $((SECONDS - start)) -ge "$public_wait" ]; then
			return 1
		fi
		echo "    proxy.golang.org may not have the tags yet: again in 60s ($(((SECONDS - start) / 60)) of $((public_wait / 60)) min waited)" >&2
		sleep 60
	done
}

work="$(mktemp -d)"
trap 'chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work"' EXIT
cache="$(go env GOMODCACHE)/cache/download"

repo="$root"
if [ -z "$proxy" ] && [ "$public" = false ]; then
	echo "==> cut $version of HEAD in a scratch clone"
	repo="$work/repo"
	git clone --quiet --shared --no-checkout "$root" "$repo"
	proxy="$work/proxy"
	(cd "$root/internal/tools" && GOWORK=off go run ./release \
		-repo "$repo" -version "$version" -commit "$(git -C "$root" rev-parse HEAD)" -proxy "$proxy") >/dev/null
fi
git -C "$repo" rev-parse --verify --quiet "refs/tags/$version^{commit}" >/dev/null ||
	fail "$repo has no tag $version"

# Every go command from here on sees only what this script sets: no go env
# file, no workspace, a module cache of its own (so no version comes from an
# earlier run), and -trimpath, so the build cache serves repeated runs.
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOPRIVATE='' GONOPROXY='' GOINSECURE=''
export GOFLAGS="-mod=mod -modcacherw -trimpath"
export GOMODCACHE="$work/gomodcache"
if [ "$public" = true ]; then
	export GOPROXY=https://proxy.golang.org GONOSUMDB='' GOSUMDB=sum.golang.org
else
	third=https://proxy.golang.org
	[ -d "$cache" ] && third="file://$cache,$third"
	export GOPROXY="file://$proxy,$third" GONOSUMDB="$module"
fi

# The modules of the release, from its tags.
args=()
while read -r tag; do
	if [ "$tag" = "$version" ]; then
		args+=("$module@$version")
	else
		args+=("$module/${tag%/"$version"}@$version")
	fi
done < <(git -C "$repo" tag --list "$version" "*/$version")
[ "${#args[@]}" -gt 1 ] || fail "$repo has no module tags */$version"

# download <name> <env...>: go mod download -json of every module, into
# $work/<name>, keeping the fields that must agree (path, version and
# hashes; a direct download has an Origin after them, hence the commas).
download() {
	local name="$1"
	shift
	env "$@" go mod download -json "${args[@]}" >"$work/$name.json" 2>"$work/$name.err" || {
		cat "$work/$name.err" >&2
		grep '"Error"' "$work/$name.json" >&2 || true
		return 1
	}
	grep -E '"(Path|Version|Sum|GoModSum)":' "$work/$name.json" | sed 's/,$//' >"$work/$name"
}

echo "==> the go command builds the same ${#args[@]} modules from the tags (GOPROXY=direct)"
printf '[url "%s"]\n\tinsteadOf = https://%s\n' "$repo" "$module" >"$work/gitconfig"
download direct GOPROXY=direct GIT_CONFIG_GLOBAL="$work/gitconfig" GIT_CONFIG_NOSYSTEM=1 \
	GONOSUMDB="$module" GOMODCACHE="$work/cache-direct" || fail "go mod download from the tags"
retry download served GOMODCACHE="$work/cache-served" || fail "go mod download from $GOPROXY"
diff -u "$work/direct" "$work/served" || fail "the release does not serve the zips the go command builds from its tags"

mkdir "$work/scratch" "$work/program"
cd "$work/scratch"
go mod init example.com/release-check >/dev/null 2>&1

echo "==> go install $module/cmd/mamori@$version"
GOBIN="$work/bin" retry go install "$module/cmd/mamori@$version" || fail "go install"
"$work/bin/mamori" version || fail "the installed CLI does not run"

# A program outside the repository: the core, a provider with an SDK of its
# own, one built on providers/httpcore (a module the program never names),
# the server and an x/ module.
echo "==> a program that uses the modules, after go get of each at $version"
cd "$work/program"
uses=(providers/aws providers/bitwarden providers/vault server x/otel)
{
	echo 'package main'
	echo
	echo 'import ('
	echo "	_ \"$module\""
	for m in "${uses[@]}"; do
		echo "	_ \"$module/$m\""
	done
	echo ')'
	echo
	echo 'func main() {}'
} >main.go
go mod init example.com/uses-mamori >/dev/null 2>&1
gets=("$module@$version")
for m in "${uses[@]}"; do
	gets+=("$module/$m@$version")
done
retry go get "${gets[@]}" || fail "go get of the modules at $version"
go mod tidy || fail "go mod tidy in the program"
go build -o program . || fail "the program does not build"
if grep -q '^replace' go.mod; then
	fail "the program needed a replace"
fi
for m in "" "${uses[@]/#//}" /providers/httpcore; do
	grep -qE "^\s+$module$m $version( // indirect)?\$" go.mod || fail "the program is not on $module$m $version"
done

echo "release-check: $version installs from outside the repository"
