#!/usr/bin/env bash
# Read-only validation. Requires Go, Git, jq, and (for tooling tests) Python 3.
set -euo pipefail
source "$(dirname "$0")/release-lib.sh"

inventory_check() {
  local path expected actual root_info
  jq -e '
    (.module | type == "string") and
    (.modules | length > 0) and
    ([.modules[].path] | length == (unique | length)) and
    ([.modules[] | select(.path == "." and .kind == "published")] | length == 1) and
    all(.modules[];
      (.path | test("^(\\.|tools|extensions/[a-z0-9_-]+|examples/[a-z0-9_-]+)$")) and
      (.kind == "published" or .kind == "example") and
      (.race | type == "boolean") and
      (.smoke | test("^(\\.|[a-z0-9_-]+)$")) and
      (if (.path | startswith("examples/")) then .kind == "example" else .kind == "published" end))
  ' "$INVENTORY" >/dev/null
  while IFS= read -r path; do
    [ -f "$ROOT/$path/go.mod" ] || { fail "missing go.mod for $path"; return 1; }
    module_path "$path" >/dev/null
  done < <(jq -r '.modules[].path' "$INVENTORY")
  expected="$(jq -c '[.modules[].path] | sort' "$INVENTORY")"
  actual="$(cd "$ROOT" && go work edit -json | jq -c '[.Use[].DiskPath | sub("^\\./"; "")] | sort')"
  [ "$actual" = "$expected" ] || { fail "go.work and module inventory differ"; return 1; }
  root_info="$(cd "$ROOT" && GOWORK=off go mod edit -json)"
  [ "$(jq '(.Require // []) | length' <<<"$root_info")" -eq 0 ] ||
    { fail "the root module must remain dependency-free"; return 1; }
}

metadata_state() {
  local file
  for file in "$ROOT/$1/go.mod" "$ROOT/$1/go.sum" "$ROOT/go.work" "$ROOT/go.work.sum"; do
    if [ -f "$file" ]; then cksum "$file"; fi
  done
}

workspace_check() (
  local path="$1" record output before
  record="$(module_record "$path")"
  before="$(metadata_state "$path")"
  cd "$ROOT/$path"
  export GOWORK="$ROOT/go.work"
  if [ "$(jq -r .kind <<<"$record")" = example ]; then
    output="$(mktemp -d)"
    trap 'rm -rf "$output"' EXIT
    go build -mod=readonly -o "$output/" ./...
  else
    go test -mod=readonly -count=1 ./...
    go vet -mod=readonly ./...
  fi
  [ "$(metadata_state "$path")" = "$before" ] ||
    { fail "module/workspace metadata changed during validation"; return 1; }
)

race_check() (
  local path="$1" record
  record="$(module_record "$path")"
  [ "$(jq -r .race <<<"$record")" = true ] || { fail "$path is not a race-test target"; return 1; }
  cd "$ROOT/$path"
  GOWORK="$ROOT/go.work" go test -mod=readonly -race -count=1 ./...
)

release_check() (
  local path="$1" info
  published_record "$path" >/dev/null
  cd "$ROOT/$path"
  export GOWORK=off
  info="$(go mod edit -json)"
  [ "$(jq '(.Replace // []) | length' <<<"$info")" -eq 0 ] ||
    { fail "published modules cannot contain replacements; fix them in a PR"; return 1; }
  [ "$(jq '(.Exclude // []) | length' <<<"$info")" -eq 0 ] ||
    { fail "dependency exclusions are ignored by consumers; resolve them in a PR"; return 1; }
  go mod tidy -diff
  go test -mod=readonly -count=1 ./...
  go vet -mod=readonly ./...
  if [ "$(published_record "$path" | jq -r .race)" = true ]; then
    go test -mod=readonly -race -count=1 ./...
  fi
)

consumer_check() (
  local path="$1" version="$2" record declared package temp attempt
  record="$(published_record "$path")"
  validate_version "$version"
  declared="$(module_path "$path")"
  package="$declared"
  [ "$(jq -r .smoke <<<"$record")" = . ] || package="$declared/$(jq -r .smoke <<<"$record")"
  temp="$(mktemp -d)"
  # Downloaded modules are read-only; make only our disposable tree removable.
  trap 'chmod -R u+w "$temp"; rm -rf "$temp"' EXIT
  cd "$temp"
  # Deliberately bypass both the checkout and setup-go's module cache.
  export GOWORK=off GOMODCACHE="$temp/modcache" GOPROXY=https://proxy.golang.org
  export GOSUMDB=sum.golang.org GOPRIVATE= GONOPROXY= GONOSUMDB= GOFLAGS=
  go mod init automata-release-smoke
  attempt=1
  until go get "$declared@$version"; do
    [ "$attempt" -lt 6 ] || { fail "published module is not resolvable after six attempts; do not move its tag"; return 1; }
    printf '==> waiting for module proxy/checksum availability (attempt %s/6)\n' "$attempt" >&2
    sleep "$((5 * (1 << (attempt - 1))))"
    attempt=$((attempt + 1))
  done
  go list -m -json "$declared" | jq -e --arg version "$version" \
    '.Version == $version and .Replace == null' >/dev/null
  printf 'package main\nimport _ "%s"\nfunc main() {}\n' "$package" >main.go
  go build -mod=readonly -o "$temp/smoke" .
  go list -m all
)

case "${1:-}" in
inventory) inventory_check ;;
matrix) jq -c '[.modules[] | {path, kind, race}]' "$INVENTORY" ;;
format)
  unformatted="$(git -C "$ROOT" ls-files -z '*.go' | (cd "$ROOT" && xargs -0 gofmt -l))"
  [ -z "$unformatted" ] || { printf '%s\n' "$unformatted"; fail "run gofmt on these files"; exit 1; }
  ;;
workspace)
  if [ "$#" -eq 2 ]; then workspace_check "$2";
  else
    while IFS= read -r path; do workspace_check "$path"; done < <(jq -r '.modules[].path' "$INVENTORY")
  fi
  ;;
race) [ "$#" -eq 2 ]; race_check "$2" ;;
release) [ "$#" -eq 2 ]; release_check "$2" ;;
plan) [ "$#" -eq 4 ]; release_plan "$2" "$3" "$4" ;;
consumer) [ "$#" -eq 3 ]; consumer_check "$2" "$3" ;;
*)
  printf 'Usage: %s inventory|matrix|format|workspace [module]|race module|release module|plan module version SHA|consumer module version\n' "$0" >&2
  exit 1
  ;;
esac
