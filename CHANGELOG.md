# Changelog

## 0.7.2

### Fixed
- **`Code.Run` `OnStdout`/`OnStderr` fire while the cell runs.** They were called only after the run finished (a 6 s cell with a tick every 2 s delivered the first output at 6.3 s). The request now carries `stream:true` when a callback is set, and a guest that streams sends output frames before the result; a character split across two frames arrives whole, and the result's complete text is delivered only beyond what the frames already did, so nothing is delivered twice. A guest that predates streaming sends no frames and its whole output is delivered once from the result, as before. Retest C-32.

### Tests
- A command that exits before its handle exists (its `cmd.exit` buffered ahead of the handle) leaves no "channel closed" hook registered, so the channel can idle-close. The order in `Commands.Start` was already right; the test pins it (retest B-N1 / C4, fixed in the TS and Python SDKs).

## 0.7.1

### Fixed
- **`Client.Pause` retries `503 PauseRetryable`.** A host that cuts a pause short leaves the sandbox running and the gateway answers a retryable 503, but `POST /pause` carried no `Idempotency-Key`, so the transport never retried and the caller saw the 503. Pause is idempotent on the gateway (an already-paused sandbox answers 200), so every call now sends one key, the same on every attempt, and the 503 is retried with backoff.

## 0.7.0

Closes two gaps the 2026-11-10 retest found in 0.5/0.6: a Go caller could not read a command's binary output, and a failed `git` call was not a `*SolariError`.

### Added
- **`CommandResult.StdoutBytes` / `StderrBytes`**: the exact output bytes, as Rust (`stdout_bytes`) and C++ already had. `Stdout` / `Stderr` stay UTF-8 text (each invalid byte becomes U+FFFD), so a command that prints binary data (an image, a tarball, `cat` of a file) is read from the byte fields. They are exact on the control socket; on REST `/exec` they are exact when the gateway sends `stdoutBase64` / `stderrBase64` (it does for output that is not valid UTF-8), and otherwise the bytes of the text.
- **`GitError`**: a `git` command the sandbox ran that exited non-zero (`Subcommand`, `ExitCode`, git's own message with any credential masked). It embeds `SolariError`, so `errors.As(err, &*SolariError)` matches every git failure.

### Changed
- **Git failures are typed.** `Git.Clone`, `Add`, `Commit`, `Push`, `Pull`, `Checkout`, `Branches`, `Log` and `Status` return `*GitError` where they returned an untyped `fmt.Errorf`; the message text is unchanged, so code that matches on it still works. Input the SDK refuses before running git (a credential containing a line break or NUL, a ref or remote starting with `-`) is now a `*ValidationError`, the same class as a bad `Commands.Run` option.

### Upgrading from 0.4 (behaviour changes since 0.5 that can break callers)
- **`Commands.Run` validates before it picks a transport (0.5.0):** a negative `TimeoutMs`, or a `User` that is not `root` or a unix user name, now returns `*ValidationError` instead of reaching the gateway.
- **A run with no `TimeoutMs` gets the 10-minute tier maximum on both paths (0.5.0).** On the control-socket path it used to have no deadline, so a long job that relied on that now needs an explicit `TimeoutMs` of up to 600000.
- **A cancellable call (a context that can be cancelled) runs over the control connection (0.5.0)**, not REST, because cancelling a REST request does not stop the guest process. Cancel errors match `errors.Is(err, context.Canceled)`.
- **`Commands.Run` connects the control channel itself (0.5.0)** when it needs it; code that called `Connect()` first still works.
- **A command that outlives its deadline is a result, not an error (0.5.0):** exit -1 with `TimedOut` true, on both paths.
- **`Git.Status`, `Branches` and `Log` parse differently (0.6.0):** status paths are verbatim (no quotes or octal escapes), `Branches` lists only real branches (no branch is `Current` on a detached HEAD), and `Log` on a repository with no commit returns an empty list instead of an error.

## 0.6.0

`Git.Status`, `Git.Branches` and `Git.Log` no longer lose information. The retest found their parsing lossy: quoted paths came back with their quotes and octal escapes, a rename was one string "old -> new", an untracked directory was listed as "dir/", a conflict showed up as both staged and modified, a detached HEAD was a pseudo branch, and `Log` kept only the subject and failed on an empty repository.

### Changed
- **`Status` runs `git status --porcelain=v1 -z --branch --untracked-files=all`.** Paths are verbatim (spaces, quotes and non-ASCII are not quoted or escaped), a rename or copy is its new path with the old one in `OrigPath`, and an untracked directory lists its files. A path git reports as unmerged (`DD AU UD UA DU AA UU`) is in `Conflicted` only, not in `Staged` or `Modified`. `Clean` is true when git reports no entries at all.
- **`Branches` uses `git for-each-ref refs/heads`**: only real branches are listed. With a detached HEAD no branch is `Current` (`Status().Detached` says so); before, `git branch` printed "(HEAD detached at abc)" as a branch named that.
- **`Log` returns an empty list for a repository with no commit** instead of an error. Any other git failure is still an error.

### Added
- `GitStatus.Upstream`, `Conflicted` and `Entries` (`[]GitStatusEntry{Path, OrigPath, Index, Worktree}`, the two XY letters per path). `Detached` is true for "HEAD (no branch)", and `Ahead`/`Behind` are 0 when the upstream is gone.
- `GitBranch.Upstream`, the tracking branch when set.
- `GitCommit.Body`: the rest of the commit message; `Message` stays the subject line.

## 0.5.0

`Commands.Run` now behaves the same whether it goes over REST (`/exec`) or the control WebSocket (`cmd.start`). Measured on staging with the same command on both paths; the only difference left is the guest's own text for a bad `Cwd` on the WebSocket (it names the binary, not the directory).

### Added
- **`CommandHandle.CloseStdin`** closes a started command's stdin (an EOF to `cat`, a shell, or anything reading it). It was internal; cancel used it.
- **`CommandResult.OOMKilled`** is true when the guest killed the command for exceeding the sandbox's memory limit (exit 137). Only the REST route reports it.
- `ConnectionError.Cause`: a request ended by your context (cancelled or expired) is now `errors.Is(err, context.Canceled)` / `context.DeadlineExceeded` as well as a `*ConnectionError`. This includes cancelling a `Run` that went over REST.

### Changed
- **`Commands.Run` validates before it picks a transport**: a negative `TimeoutMs` or a `User` that is not `root` or a unix user name returns the new `*ValidationError` (HTTP 400 semantics: it embeds `*GatewayError`, `Status` 400, `Code` `ValidationError`) without sending anything. Every gateway 400 is now a `*ValidationError` (still matches `*GatewayError`).
- **A run with no `TimeoutMs` gets the tier maximum (10 minutes) on both paths**; before, the control-socket path had no deadline. Go has no way to ask for "no deadline": `TimeoutMs` 0 means "unset" (so the 10 minute maximum applies) and a negative value is rejected, unlike the TypeScript and Python SDKs where 0 means none. A command that must run longer than 10 minutes should be started with `Commands.Start` and waited on with `Wait`. A deadline under one second runs over the control socket, where it is enforced client-side.
- **A cancellable call uses the control connection.** Cancelling a `POST /exec` request does not stop the process in the guest, whereas cancelling a run on the connection sends `cmd.kill`. A `Run` whose context can be cancelled (`ctx.Done() != nil`) therefore connects and runs over the socket; a plain `context.Background()` run still takes the one-shot REST route. A `shared` sandbox refuses command streams, so there REST is the only route and a cancellable call uses it too (cancelling abandons the request; it cannot stop the guest process).
- A REST `409 HostCapabilityMissing` from `/exec` (a host pool without per-user exec) is retried once over the control connection.
- **A command that outlives its deadline over REST is a result, not an error**: exit -1, `TimedOut`, as over the control socket. The gateway answers 400 `ExecTimeout`, or 502 with an `i/o timeout` message when its read of the guest timed out first; both used to surface as `*GatewayError`.
- The REST `/exec` body always carries `args` (an empty array when there are none), as the other SDKs send it.
- **`Commands.Run` connects the control channel itself** when the run needs it (`Env`, `User`, output callbacks, `Background`). It used to return `ConnectionError "Not connected - call Connect() first"` unless you had called `Connect()`; a plain run over REST never needed it. `Commands.Start` still needs an open channel.
- **A command that cannot start is one error on both paths**: `*ActionError{Method: "cmd.start", Code: "ExecFailed"}` with the real cause in the message (`fork/exec ...`, `chdir ...`). REST used to return a `*GatewayError` (status 400, code `ExecFailed`) and the WebSocket an `*ActionError` with no code. Code that matched the old `*GatewayError` for this case should match `*ActionError` and `Code == "ExecFailed"`.
- **`CommandResult.Stdout` / `Stderr` are decoded over the whole stream**: a character split across output chunks survives, and each invalid byte becomes U+FFFD, as the REST route already returned. Over the WebSocket they used to hold the raw invalid bytes. The `OnStdout` / `OnStderr` callbacks still get each chunk as it arrives.

## 0.4.0

### Added
- **`SandboxNotStoppedError`** for `409 NotStopped` and `409 StartInProgress` from `Client.Start`, the same class the TypeScript and Python SDKs expose. It embeds `*GatewayError` (so `errors.As(&*GatewayError)` still matches, `Status` and `Code` are unchanged) and the sandbox's current state is in `State`. `NotStopped` means the sandbox is in another state (use `Resume` for a paused one); `StartInProgress` means a start is already under way, so poll `Get`. Other 409s (a stopped disk that cannot start, `HostCapabilityMissing`) stay plain `*GatewayError`.
- `GatewayErrorBody.State`.

### Tests
- The shared SDK conformance rows now run in CI (`tests/conformance`), including one gated live smoke row.

## 0.3.4

### Fixed
- **`Commands.Run` on a shared-isolation sandbox works when the control channel is connected.** A shared sandbox refuses command streams (`cmd.start`, error `NotAvailableOnSharedIsolation`) but runs commands over REST; `Run` on a connected channel sent `cmd.start` and returned the refusal. It now runs that command over REST `/exec` and remembers that the sandbox is shared, so later runs go straight to REST (the same behaviour as the TypeScript and Python SDKs). A run that needs a stream (an output callback, `Env`, `User`, `Background`) still returns the refusal.

## 0.3.3

### Added
- **`Files.Upload` and `Files.Download`** move a file in 4 MiB chunks (`fs.upload` / `fs.download`), so a large file never travels as one frame. A 48 MiB file used to be a 64 MiB frame that closed the control channel and failed every call in flight on it. `Files.Read` now reads through the same chunked loop (a guest image that predates `fs.download` is read with `fs.read`).

### Changed
- `Files.Write` refuses more than 32 MiB before sending anything (`file too large for a single call (N bytes, max 33554432): use files.upload()/files.download() or uploadUrl()`), so one big call fails alone and the channel stays up. Use `Upload` for larger files.

### Fixed
- **Cancelling `Commands.Run` (a cancelled context) stops the guest process.** It gets SIGTERM and a closed stdin at once and SIGKILL 2 s later if it is still running; before, it ran on. `Code.Run` asks the guest to interrupt the kernel (`code.interrupt`; a guest that does not know it is ignored).
- **`CommandHandle.Wait` can be called more than once, and from several goroutines;** the second call used to block until its context ended.
- **A callback may make calls.** Output callbacks ran on the control socket's read loop, so a callback that called `Stdin`, `Kill` or read a file stalled the channel until the call timed out. Callbacks now run on a goroutine per command, in order, and `Wait` returns after the last one.
- **Concurrent `Connect` calls open one socket,** not one each (the extras leaked with their reader goroutines).

## 0.3.2

### Fixed
- **A deleted sandbox no longer reports "call Connect() first".** A `404` on the one-shot `/exec` route is only read as "route not served" (and retried over the control channel) when it is a plain-text 404; a JSON `404` is the gateway saying the sandbox is gone, and now reaches you as that error.
- **`CommandResult.TimedOut`** says a command hit its `TimeoutMs` and was killed (`ExitCode` -1). The one-shot route reported it and it was dropped; the control-channel path now sets it too.
- **`Sandbox.RecordingURL`** and **`CreateSandboxResponse.RecordingURL`** carry the recording link from a create that asked for a recording (it was discarded).
- **Security: `Git.Clone` / `Push` / `Pull` credentials no longer reach the clone URL, `.git/config` or the process list.** `Clone` with a username/password used to put them in the URL, so git saved them as `remote.origin.url` (and from there into snapshots and templates) and showed them in `/proc/*/cmdline`; push and pull put them in a `-c url.….insteadOf=` argument. They now travel as environment variables of that one command and git reads them through an inline credential helper bound to the remote's origin, so nothing is left in the repository, in a config file or on a command line, and git never offers the password to another host (a redirect to a different host gets nothing). If you cloned a private repo with credentials before this release, check `.git/config` in existing snapshots and templates and rotate the token if it is there. A username or password with a line break is refused, and the password is masked in error messages. Authenticated git needs the live control connection, so on a `shared` sandbox only public remotes work.
- **git commands no longer read user text as options.** `Clone`, `Push`, `Pull` and `Checkout` put `--` before the repository, remote and ref, and refuse a remote, branch or ref that starts with `-` (`Clone("--upload-pack=cmd", …)` ran `cmd` in the sandbox; `Checkout("--orphan=x")` created a branch).

## 0.3.1

### Changed
- `ClientOptions.BaseURL` is optional: empty means the hosted Solari API (`DefaultBaseURL`,
  `https://api.getsolari.com`), matching the TypeScript and Python SDKs. An explicit `BaseURL` is used as before.
