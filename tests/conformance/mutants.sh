#!/usr/bin/env bash
# Mutation check: break one thing in the binding, the matching conformance row(s) must turn red; the tree is restored after each.
# Usage: tests/conformance/mutants.sh   (from the repo root; needs a clean tree for the files it edits)
set -uo pipefail
cd "$(dirname "$0")/../.."
run() { # name file sed-expression
  local name=$1 file=$2 expr=$3
  cp "$file" "$file.orig"; sed -i "$expr" "$file"
  if cmp -s "$file" "$file.orig"; then echo "MUTANT $name: sed changed nothing"; mv "$file.orig" "$file"; return 1; fi
  local out; out=$(go test ./tests/conformance/ 2>&1); local rc=$?
  mv "$file.orig" "$file"
  if [ $rc -eq 0 ]; then echo "MUTANT $name: SURVIVED (suite stayed green)"; return 1; fi
  echo "MUTANT $name: killed by $(echo "$out" | grep -c -- '--- FAIL: TestConformanceRows/') row(s):"; echo "$out" | grep -- '--- FAIL: TestConformanceRows/' | sed 's/.*TestConformanceRows\//    /; s/ (.*//' | head -6
}
bad=0
run "get uses the wrong path"            client.go 's#"/sandboxes/"+encodeURIComponent(sandboxID), nil, httpRequestOptions{}, \&view#"/sandbox/"+encodeURIComponent(sandboxID), nil, httpRequestOptions{}, \&view#' || bad=1
run "create drops the idempotency key"   client.go 's#idempotencyKey: newIdempotencyKey(),#idempotencyKey: "",#' || bad=1
run "kill does not escape the id"        client.go '/func (c \*Client) Kill/,/^}/ s#encodeURIComponent(sandboxID)#sandboxID#' || bad=1
run "401 no longer an AuthError"         errors.go '0,/AuthError{/ s#AuthError{#PlanError{#' || bad=1
exit $bad
