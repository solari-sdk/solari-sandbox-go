# Changelog

## 0.3.2

### Fixed
- **Security: `Git.Clone` / `Push` / `Pull` credentials no longer reach the clone URL, `.git/config` or the process list.** `Clone` with a username/password used to put them in the URL, so git saved them as `remote.origin.url` (and from there into snapshots and templates) and showed them in `/proc/*/cmdline`; push and pull put them in a `-c url.….insteadOf=` argument. They now travel as environment variables of that one command and git reads them through an inline credential helper, so nothing is left in the repository, in a config file or on a command line. If you cloned a private repo with credentials before this release, check `.git/config` in existing snapshots and templates and rotate the token if it is there. A username or password with a line break is refused, and the password is masked in error messages. Authenticated git needs the live control connection, so on a `shared` sandbox only public remotes work.
- **git commands no longer read user text as options.** `Clone`, `Push`, `Pull` and `Checkout` put `--` before the repository, remote and ref, and refuse a remote, branch or ref that starts with `-` (`Clone("--upload-pack=cmd", …)` ran `cmd` in the sandbox; `Checkout("--orphan=x")` created a branch).

## 0.3.1

### Changed
- `ClientOptions.BaseURL` is optional: empty means the hosted Solari API (`DefaultBaseURL`,
  `https://api.getsolari.com`), matching the TypeScript and Python SDKs. An explicit `BaseURL` is used as before.
