#!/usr/bin/env bash
# Fails if a non-standard package of the production build uses cgo (§25.4).
set -euo pipefail
pkgs=$(CGO_ENABLED=1 go list -deps -f '{{if and (not .Standard) .CgoFiles}}{{.ImportPath}}{{end}}' ./...)
if [ -n "$pkgs" ]; then
  echo "packages with cgo files:" >&2
  echo "$pkgs" >&2
  exit 1
fi
echo "no non-standard package uses cgo"
