# Changelog

## 0.5.0

`Commands.Run` now behaves the same whether it goes over REST (`/exec`) or the control WebSocket (`cmd.start`). Measured on staging with the same command on both paths; the only difference left is the guest's own text for a bad `Cwd` on the WebSocket (it names the binary, not the directory).

### Changed
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
- **Cancelling `Commands.Run` (a cancelled context) stops the guest process.** It gets SIGTERM and a closed stdin at once and SIGKILL 2 s later if it is still running; before, it ran on. `Code.Run` asks the guest to interrupt the kernel (`code.interrupt`; a guest that does not know it is ignored). The one-shot `/exec` path needs nothing: the gateway kills the process when the request goes away.
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
