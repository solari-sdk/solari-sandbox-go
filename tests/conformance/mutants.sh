#!/usr/bin/env bash
# Mutation check: break one thing in the binding, the matching conformance row(s) must turn red; the tree is restored after each.
# Usage: tests/conformance/mutants.sh   (from the repo root; needs a clean tree for the files it edits)
set -uo pipefail
cd "$(dirname "$0")/../.."
run() { # name file sed-expression
  local name=$1 file=$2 expr=$3
  cp "$file" "$file.orig"; sed -i "$expr" "$file"
  if cmp -s "$file" "$file.orig"; then echo "MUTANT $name: sed changed nothing"; mv "$file.orig" "$file"; return 1; fi
  local out; out=$(go test -count=1 ./... 2>&1); local rc=$?
  mv "$file.orig" "$file"
  if [ $rc -eq 0 ]; then echo "MUTANT $name: SURVIVED (suite stayed green)"; return 1; fi
  echo "MUTANT $name: killed by $(echo "$out" | grep -c -- '--- FAIL: TestConformanceRows/') conformance row(s) and $(echo "$out" | grep -c -- '^--- FAIL: ') unit test(s):"; echo "$out" | grep -- '--- FAIL: ' | grep -v 'TestConformanceRows[ /(]*[0-9.s)]*$' | sed 's/.*--- FAIL: //; s/TestConformanceRows\///; s/ (.*//; s/^/    /' | head -6
}
bad=0
run "get uses the wrong path"            client.go 's#"/sandboxes/"+encodeURIComponent(sandboxID), nil, httpRequestOptions{}, \&view#"/sandbox/"+encodeURIComponent(sandboxID), nil, httpRequestOptions{}, \&view#' || bad=1
run "create drops the idempotency key"   client.go 's#idempotencyKey: newIdempotencyKey(),#idempotencyKey: "",#' || bad=1
run "kill does not escape the id"        client.go '/func (c \*Client) Kill/,/^}/ s#encodeURIComponent(sandboxID)#sandboxID#' || bad=1
run "401 no longer an AuthError"         errors.go '0,/AuthError{/ s#AuthError{#PlanError{#' || bad=1
run "exec 400 ExecTimeout is an error"    client.go 's#ge.Status == 400 \&\& ge.Code == "ExecTimeout"#false#' || bad=1
run "exec 502 i/o timeout is an error"    client.go 's#ge.Status == 502 \&\& strings.Contains(ge.Message, "i/o timeout")#false#' || bad=1
run "exec drops the empty args array"     client.go 's#req.Args = \[\]string{} // always sent.*#req.Args = nil#' || bad=1
run "oomKilled is not read"               types.go  's#`json:"oomKilled"`#`json:"oom_killed"`#' || bad=1
run "cancel is not errors.Is(Canceled)"   http.go   's#ce.Cause = cerr.*#_ = cerr#' || bad=1
run "negative timeout is not rejected"    commands.go 's#if timeoutMs < 0 {#if false {#' || bad=1
run "bad user is not rejected"            commands.go 's#!validUser(opts.User)#false#' || bad=1
run "HostCapabilityMissing is not retried" commands.go 's#!isRouteUnavailable(err) \&\& !isHostCapabilityMissing(err)#!isRouteUnavailable(err)#' || bad=1
run "cancellable call still uses REST"    commands.go 's#restPlain \&\& (ctx.Done() == nil || s.shared.Load())#restPlain#' || bad=1
run "status drops -z"                     git.go 's#"--porcelain=v1", "-z", "--branch"#"--porcelain=v1", "--branch"#' || bad=1
run "status lists untracked dirs, not files" git.go 's#"--untracked-files=all"}#"--untracked-files=normal"}#' || bad=1
run "conflicts are staged/modified too"   git.go 's#case gitConflictCodes\[xy\]:#case false:#' || bad=1
run "rename loses the old path"           git.go 's#entry.OrigPath = recs\[i\]#entry.OrigPath = ""#' || bad=1
run "branches use git branch"             git.go 's#"for-each-ref", "--format#"branch", "--format#' || bad=1
run "log drops the body"                  git.go 's#Body:    strings.TrimRight(field(fields, 5), "\\n"),#Body:    "",#' || bad=1
run "empty repo log is an error"          git.go 's#strings.Contains(err.Error(), "does not have any commits yet")#false#' || bad=1
exit $bad
