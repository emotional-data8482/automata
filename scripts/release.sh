#!/usr/bin/env bash
# CI-only, module-scoped publication. Does not edit files or commit branches.
# Invoke only from the approved release workflow, never as a local check.
set -euo pipefail
source "$(dirname "$0")/release-lib.sh"

if [ "$#" -ne 3 ]; then
  printf 'Usage: %s module exact-version tested-commit-SHA\nRun the Release module workflow on GitHub; local publication is disabled.\n' "$0" >&2
  exit 1
fi
publish_release "$1" "$2" "$3"
