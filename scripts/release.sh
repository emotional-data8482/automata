#!/usr/bin/env bash
#
# Release automation for the automata multi-module repository.
#
# One command bumps every submodule's `automata` require to the new version,
# verifies the whole workspace, commits, tags the root module and every
# published submodule, and (with --push) pushes and refreshes the go.sum files
# against the published tags.
#
# Usage:
#   scripts/release.sh v0.5.0 [--push]
#   scripts/release.sh patch|minor|major [--push]
#   scripts/release.sh refresh-sums --push   (re-run just the post-push tidy)
#
# Why go.sum refresh happens after push: `go mod tidy` in a submodule needs the
# new automata version to be resolvable, and the Go proxy only serves it once
# the root tag is on the remote. Tags created locally are not enough (Go
# resolves through the proxy, not the local repo). So the flow is two commits:
# the release commit (which the tags point at) and a small go.sum-refresh
# commit afterwards. Consumers are unaffected either way — they verify
# dependencies against their own go.sum and the checksum database, not a
# library's.
#
# Examples are bumped too (cosmetically — they carry replace directives and are
# never tagged), so their require lines don't drift into confusion.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODULE="github.com/emotional-data8482/automata"
TOOLS_MODULE="$MODULE/tools"
# Published modules: bumped, tested, tidied, and tagged on every release. A
# published module may keep a local automata replace while it depends on
# unreleased core (so `go mod tidy` and `go get` work in it); the release drops
# that replace and refuses to tag any other.
PUBLISHED=(tools extensions/claude extensions/openai extensions/openrouter extensions/tavily extensions/sqlite)
# Modules that ride along but are neither tagged nor tidied.
RIDEALONG=(examples/claude examples/openai examples/deep_research examples/durable_typed examples/durable_host)
TOOLS_CONSUMERS=(extensions/tavily examples/deep_research)

usage() {
  sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
}

die() {
  echo "error: $*" >&2
  exit 1
}

NEW="${1:-}"
PUSH="${2:-}"

case "$PUSH" in "" | "--push") ;; *) usage ;; esac

# refresh-sums: run the post-push phase standalone (tidy published modules
# against tags that now exist on the remote, commit + push the result).
if [ "$NEW" = "refresh-sums" ]; then
  [ "$PUSH" = "--push" ] || die "usage: scripts/release.sh refresh-sums --push"
  git -C "$ROOT" diff --quiet || die "working tree has unstaged changes"
  for m in "${PUBLISHED[@]}"; do
    echo "==> tidy: $m"
    (cd "$ROOT/$m" && go mod tidy)
  done
  (cd "$ROOT" && go build ./... >/dev/null)
  if ! git -C "$ROOT" diff --quiet || ! git -C "$ROOT" diff --cached --quiet; then
    git -C "$ROOT" add -A
    git -C "$ROOT" commit -m "Refresh go.sum files"
    git -C "$ROOT" push origin "$(git -C "$ROOT" branch --show-current)"
  else
    echo "==> go.sum files already up to date"
  fi
  exit 0
fi

# --- resolve the new version ------------------------------------------------

bump() { # bump <major> <minor> <patch> <part>
  local major=$1 minor=$2 patch=$3 part=$4
  case "$part" in
  major) echo "v$((major + 1)).0.0" ;;
  minor) echo "v$major.$((minor + 1)).0" ;;
  patch) echo "v$major.$minor.$((patch + 1))" ;;
  esac
}

case "$NEW" in
major | minor | patch)
  LATEST="$(git -C "$ROOT" describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null)" ||
    die "no root version tag found; pass an explicit version (e.g. v0.4.0)"
  LATEST="${LATEST#v}"
  IFS=. read -r MA MI PA <<EOF
$LATEST
EOF
  NEW="$(bump "$MA" "$MI" "$PA" "$1")"
  ;;
v[0-9]*.[0-9]*.[0-9]*) ;;
*) usage ;;
esac

echo "==> releasing $NEW"

# --- preflight checks -------------------------------------------------------

git -C "$ROOT" diff --quiet || die "working tree has unstaged changes"
git -C "$ROOT" diff --cached --quiet || die "working tree has staged changes"
git -C "$ROOT" rev-parse -q --verify "refs/tags/$NEW" >/dev/null &&
  die "tag $NEW already exists"
BRANCH="$(git -C "$ROOT" branch --show-current)"

# --- bump the require lines -------------------------------------------------

for m in "${PUBLISHED[@]}" "${RIDEALONG[@]}"; do
  if grep -qE "$MODULE v[0-9]+\.[0-9]+\.[0-9]+" "$ROOT/$m/go.mod"; then
    sed -i.bak -E "s|$MODULE v[0-9]+\.[0-9]+\.[0-9]+|$MODULE $NEW|" "$ROOT/$m/go.mod"
    rm -f "$ROOT/$m/go.mod.bak"
    echo "    bumped $m"
  fi
done
# Tavily is published against tools, and deep_research uses it directly.
# Bump those pins together with the tools tag for this release.
for m in "${TOOLS_CONSUMERS[@]}"; do
  if grep -qE "$TOOLS_MODULE v[0-9]+\.[0-9]+\.[0-9]+" "$ROOT/$m/go.mod"; then
    sed -i.bak -E "s|$TOOLS_MODULE v[0-9]+\.[0-9]+\.[0-9]+|$TOOLS_MODULE $NEW|" "$ROOT/$m/go.mod"
    rm -f "$ROOT/$m/go.mod.bak"
    echo "    bumped tools in $m"
  fi
done

# Consumers ignore replace directives in a dependency, and the GOWORK=off
# published-pin check below would silently resolve through one.
for m in "${PUBLISHED[@]}"; do
  cp "$ROOT/$m/go.mod" "$ROOT/$m/go.mod.bak"
  (cd "$ROOT/$m" && go mod edit -dropreplace="$MODULE")
  cmp -s "$ROOT/$m/go.mod" "$ROOT/$m/go.mod.bak" || echo "    dropped automata replace in $m"
  rm -f "$ROOT/$m/go.mod.bak"
  if (cd "$ROOT/$m" && go mod edit -json) | grep -q '"Replace": \['; then
    die "$m has replace directives; published modules must not"
  fi
done

# --- verify the whole workspace (local resolution; no tags needed yet) ------

echo "==> build + test: root module"
(cd "$ROOT" && go build ./... && go test ./... >/dev/null) ||
  die "root module verification failed"

for m in "${PUBLISHED[@]}"; do
  echo "==> build + test: $m"
  (cd "$ROOT/$m" && go build ./... && go test ./... >/dev/null) ||
    die "$m verification failed"
done
for m in "${RIDEALONG[@]}"; do
  echo "==> build: $m"
  (cd "$ROOT/$m" && go build ./...) || die "$m build failed"
done

# --- commit and tag ---------------------------------------------------------

TAGS=("$NEW")
for m in "${PUBLISHED[@]}"; do
  TAGS+=("$m/$NEW") # Go multi-module tagging: <module-path>/<version>
done

git -C "$ROOT" add -A
git -C "$ROOT" commit -m "Release $NEW"
for t in "${TAGS[@]}"; do
  git -C "$ROOT" tag "$t"
  echo "    tagged $t"
done

# --- push -------------------------------------------------------------------

if [ "$PUSH" != "--push" ]; then
  echo
  echo "==> ready. review, then run with --push to publish:"
  echo "    git push origin $BRANCH ${TAGS[*]}"
  echo "    # afterwards refresh sums: scripts/release.sh refresh-sums --push"
  exit 0
fi

git -C "$ROOT" push origin "$BRANCH" "${TAGS[@]}"

# --- refresh go.sum files against the published tags ------------------------

for m in "${PUBLISHED[@]}"; do
  echo "==> tidy: $m"
  (cd "$ROOT/$m" && go mod tidy)
done
(cd "$ROOT" && go build ./... >/dev/null) # refreshes go.work.sum

if ! git -C "$ROOT" diff --quiet || ! git -C "$ROOT" diff --cached --quiet; then
  git -C "$ROOT" add -A
  git -C "$ROOT" commit -m "Refresh go.sum after $NEW"
  git -C "$ROOT" push origin "$BRANCH"
fi

# --- consumer-view verification ---------------------------------------------

for m in "${PUBLISHED[@]}"; do
  echo "==> published-pin check: $m"
  (cd "$ROOT/$m" && GOWORK=off go build ./...) ||
    die "$m does not build against published $NEW"
done

echo
echo "==> $NEW released."
