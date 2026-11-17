#!/usr/bin/env bash
# Keeps the modules of this repository on one version.
#
# The repository is one root module (github.com/xavidop/mamori) and many
# nested ones (providers/aws, providers/httpcore, x/otel, server, cmd/mamori,
# ...). A nested module requires the root, and some require each other, each
# with a `replace` to the directory beside it so the repository builds as it
# is. A program that uses a nested module does not see those replaces: it
# needs the version the require names to exist, as the tag
# `<directory>/v<version>`.
#
#   scripts/module-versions.sh set 1.14.0    # every in-repository require names v1.14.0
#   scripts/module-versions.sh tag 1.14.0    # tag providers/aws/v1.14.0, ... on HEAD
#   scripts/module-versions.sh check         # the requires agree and each has its replace
#   scripts/module-versions.sh list          # the nested modules
#
# A release runs `set` before the release commit and `tag` on it (see
# .releaserc.json and .github/workflows/release.yml), so the root tag and the
# tags of the nested modules name the same commit.
set -euo pipefail

cd "$(dirname "$0")/.."
root_path="$(sed -n 's/^module //p' go.mod)"

# The directories of the nested modules.
nested() {
	find . -name go.mod -not -path './go.mod' -not -path './site/*' -not -path '*/testdata/*' -not -path '*/node_modules/*' |
		sed 's|^\./||; s|/go\.mod$||' | sort
}

# The module path of a directory.
path_of() {
	if [ "$1" = "." ]; then echo "$root_path"; else echo "$root_path/$1"; fi
}

# in_repo_requires DIR: "path version" of every module of this repository
# that DIR requires.
in_repo_requires() {
	awk -v root="$root_path" '
		$1 == "require" && $2 == "(" { block = 1; next }
		block && $1 == ")" { block = 0; next }
		{
			path = ""; version = ""
			if (block) { path = $1; version = $2 }
			else if ($1 == "require") { path = $2; version = $3 }
			if (path == root || index(path, root "/") == 1) print path, version
		}
	' "$1/go.mod"
}

# The modules of this repository by path, to tell a require of one of them
# from a require of something that merely shares the prefix.
known="$({ echo "$root_path"; nested | while read -r d; do path_of "$d"; done; })"
is_known() { grep -qxF "$1" <<<"$known"; }

version_arg() {
	local v="${1:-}"
	v="${v#v}"
	if ! [[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
		echo "module-versions: want a version such as 1.14.0, have '${1:-}'" >&2
		exit 2
	fi
	echo "$v"
}

case "${1:-}" in
list)
	nested
	;;
set)
	version="$(version_arg "${2:-}")"
	nested | while read -r dir; do
		in_repo_requires "$dir" | while read -r path _; do
			is_known "$path" || continue
			go mod edit -require="$path@v$version" "$dir/go.mod"
		done
	done
	echo "module-versions: every in-repository require names v$version"
	;;
check)
	status=0
	versions="$(nested | while read -r dir; do in_repo_requires "$dir"; done | awk '{print $2}' | sort -u)"
	if [ "$(wc -l <<<"$versions" | tr -d ' ')" != "1" ]; then
		echo "module-versions: the in-repository requires name more than one version:" >&2
		printf '  %s\n' $versions >&2
		status=1
	fi
	while read -r dir; do
		while read -r path _; do
			is_known "$path" || { echo "module-versions: $dir requires $path, which is no module of this repository" >&2; status=1; continue; }
			if ! grep -qE "^replace[[:space:]]+${path}[[:space:]]+=>[[:space:]]+\.\.?(/|[[:space:]]*$)" "$dir/go.mod" &&
				! awk -v p="$path" '$1 == "replace" && $2 == "(" { b = 1; next } b && $1 == ")" { b = 0 } b && $1 == p && $2 == "=>" && $3 ~ /^\.\.?(\/|$)/ { found = 1 } END { exit !found }' "$dir/go.mod"; then
				echo "module-versions: $dir requires $path without a replace to its directory" >&2
				status=1
			fi
		done < <(in_repo_requires "$dir")
	done < <(nested)
	[ "$status" = 0 ] && echo "module-versions: $(nested | wc -l | tr -d ' ') nested modules, in-repository requires at $versions, each with its replace"
	exit "$status"
	;;
tag)
	version="$(version_arg "${2:-}")"
	tags=()
	while read -r dir; do
		tag="$dir/v$version"
		if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
			echo "module-versions: the tag $tag exists already" >&2
			exit 1
		fi
		tags+=("$tag")
	done < <(nested)
	for tag in "${tags[@]}"; do
		git tag "$tag"
	done
	if [ "${3:-}" = "--push" ]; then
		git push --atomic origin "${tags[@]/#/refs/tags/}"
	fi
	echo "module-versions: tagged ${#tags[@]} modules at v$version on $(git rev-parse --short HEAD)"
	;;
*)
	sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
	;;
esac
