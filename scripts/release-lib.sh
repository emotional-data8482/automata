#!/usr/bin/env bash
# Shared, side-effect-free definitions. Publication is only invoked by release.sh.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INVENTORY="$ROOT/scripts/modules.json"

fail() { printf 'error: %s\n' "$*" >&2; return 1; }

module_record() {
  jq -ce --arg path "$1" '.modules[] | select(.path == $path)' "$INVENTORY" ||
    fail "unknown module: $1"
}

published_record() {
  local record
  record="$(module_record "$1")" || return 1
  [ "$(jq -r .kind <<<"$record")" = published ] ||
    { fail "examples are not published: $1"; return 1; }
  printf '%s\n' "$record"
}

module_path() {
  local actual expected
  expected="$(jq -r .module "$INVENTORY")"
  [ "$1" = . ] || expected="$expected/$1"
  actual="$(cd "$ROOT/$1" && GOWORK=off go mod edit -json | jq -r .Module.Path)" || return 1
  [ "$actual" = "$expected" ] ||
    { fail "module path mismatch in $1: expected $expected, got $actual"; return 1; }
  printf '%s\n' "$actual"
}

validate_version() {
  [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
    { fail "use an exact stable version, e.g. v0.5.3 (not a bump or prerelease)"; return 1; }
  # Current module paths have no /vN suffix; v2+ needs an explicit API migration.
  [[ "$1" =~ ^v[01]\. ]] ||
    { fail "v2+ requires a major-version module path migration"; return 1; }
}

# Highest stable tag for this module, optionally below a candidate version.
latest_tag() {
  local prefix="" ceiling="${2:-}"
  [ "$1" = . ] || prefix="$1/"
  git -C "$ROOT" tag --list | jq -Rnr --arg prefix "$prefix" --arg ceiling "$ceiling" '
    def version: ltrimstr("v") | split(".") | map(tonumber);
    [inputs | select(startswith($prefix)) | ltrimstr($prefix)
      | select(test("^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"))
      | {tag: ($prefix + .), version: version}
      | select($ceiling == "" or .version < ($ceiling | version))]
    | sort_by(.version) | last.tag // ""'
}

release_plan() {
  local path="$1" version="$2" commit="$3" prefix="" tag existing latest previous declared checkout_status
  published_record "$path" >/dev/null || return 1
  validate_version "$version" || return 1
  [[ "$commit" =~ ^[0-9a-f]{40}$ ]] ||
    { fail "commit must be a full lowercase commit SHA"; return 1; }
  [ "$(git -C "$ROOT" rev-parse HEAD)" = "$commit" ] ||
    { fail "checkout must match the requested commit"; return 1; }
  checkout_status="$(git -C "$ROOT" status --porcelain --untracked-files=all)" || return 1
  [ -z "$checkout_status" ] ||
    { fail "checkout has tracked or untracked changes"; return 1; }
  git -C "$ROOT" merge-base --is-ancestor "$commit" "origin/${RELEASE_DEFAULT_BRANCH:-main}" ||
    { fail "commit must belong to the default branch"; return 1; }
  declared="$(module_path "$path")" || return 1
  [ "$path" = . ] || prefix="$path/"
  tag="$prefix$version"
  existing="$(git -C "$ROOT" rev-parse --verify "refs/tags/$tag^{commit}" 2>/dev/null || true)"
  if [ -n "$existing" ]; then
    [ "$existing" = "$commit" ] ||
      { fail "tag $tag already points to another commit"; return 1; }
  else
    latest="$(latest_tag "$path")" || return 1
    if [ -n "$latest" ]; then
      jq -en --arg new "$version" --arg old "${latest#"$prefix"}" '
        def version: ltrimstr("v") | split(".") | map(tonumber);
        ($new | version) > ($old | version)' >/dev/null ||
        { fail "version must be newer than $latest"; return 1; }
      git -C "$ROOT" merge-base --is-ancestor "refs/tags/$latest^{commit}" "$commit" ||
        { fail "commit must include the previous module release $latest"; return 1; }
    fi
  fi
  previous="$(latest_tag "$path" "$version")" || return 1
  jq -n --arg module "$path" --arg module_path "$declared" --arg version "$version" \
    --arg tag "$tag" --arg commit "$commit" --arg previous_tag "$previous" \
    '{module: $module, module_path: $module_path, version: $version,
      tag: $tag, commit: $commit, previous_tag: $previous_tag}'
}

release_notes() {
  local plan="$1" path commit previous range item
  local paths=()
  path="$(jq -r .module <<<"$plan")"
  commit="$(jq -r .commit <<<"$plan")"
  previous="$(jq -r .previous_tag <<<"$plan")"
  range="$commit"
  [ -z "$previous" ] || range="$previous..$commit"
  paths+=("$path")
  if [ "$path" = . ]; then
    while IFS= read -r item; do paths+=(":(exclude)$item"); done < <(
      jq -r '.modules[] | select(.path != ".") | .path' "$INVENTORY")
  fi
  printf '# %s\n\nCommit: `%s`\n\n' "$(jq -r .tag <<<"$plan")" "$commit"
  printf 'Install:\n\n```sh\ngo get %s@%s\n```\n\n## Changes\n\n' \
    "$(jq -r .module_path <<<"$plan")" "$(jq -r .version <<<"$plan")"
  git -C "$ROOT" log --format='- %s (%h)' "$range" -- "${paths[@]}"
  printf '\nInternal dependencies retain their minimum compatible versions; this release does not upgrade other modules automatically.\n'
}

# Used only after the workflow's read-only validation and environment approval.
# Tests call this definition with mocked git/gh; they never run release.sh.
publish_release() (
  local path="$1" version="$2" commit="$3" repo plan tag refs status remote_sha notes protected
  [ "${GITHUB_ACTIONS:-}" = true ] && [ "${AUTOMATA_RELEASE_APPROVED:-}" = true ] ||
    { fail "publication is only allowed in the approved GitHub Actions job"; return 1; }
  [ "${GITHUB_REF:-}" = "refs/heads/${RELEASE_DEFAULT_BRANCH:-main}" ] ||
    { fail "dispatch releases from the default branch"; return 1; }
  repo="$(jq -r '.module | ltrimstr("github.com/")' "$INVENTORY")"
  [ "${GITHUB_REPOSITORY:-}" = "$repo" ] ||
    { fail "publication is only allowed in $repo (not a fork)"; return 1; }
  protected="$(gh api "repos/$repo/environments/release" --jq \
    'any(.protection_rules[]?; .type == "required_reviewers" and ((.reviewers // []) | length) > 0)')" || return 1
  [ "$protected" = true ] ||
    { fail "configure a required reviewer on the release environment before publishing"; return 1; }
  plan="$(release_plan "$path" "$version" "$commit")" || return 1
  tag="$(jq -r .tag <<<"$plan")"
  notes="$(mktemp)" || return 1
  trap 'rm -f "$notes"' EXIT
  release_notes "$plan" >"$notes" || return 1

  # Read the remote immediately before publication, including annotated tags.
  if refs="$(git -C "$ROOT" ls-remote --exit-code origin "refs/tags/$tag" "refs/tags/$tag^{}")"; then
    remote_sha="$(awk -v ref="refs/tags/$tag^{}" '$2 == ref { print $1 }' <<<"$refs")"
    [ -n "$remote_sha" ] || remote_sha="$(awk -v ref="refs/tags/$tag" '$2 == ref { print $1 }' <<<"$refs")"
    [ "$remote_sha" = "$commit" ] ||
      { fail "remote tag $tag points to another commit; tags are immutable"; return 1; }
  else
    status=$?
    [ "$status" -eq 2 ] || { fail "could not inspect remote tags"; return 1; }
    # No local tag, branch push, source edit, or force update is necessary.
    git -C "$ROOT" push origin "$commit:refs/tags/$tag" || return 1
  fi

  if gh release view "$tag" --repo "$repo" >/dev/null 2>&1; then
    printf '==> GitHub Release %s already exists\n' "$tag"
  else
    gh release create "$tag" --repo "$repo" --verify-tag --target "$commit" \
      --title "$tag" --notes-file "$notes" --latest=false || return 1
  fi
  printf '==> published %s at %s; consumer verification follows in the workflow\n' "$tag" "$commit"
)
