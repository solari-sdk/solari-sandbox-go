# Changelog

## 0.3.1

### Changed
- `ClientOptions.BaseURL` is optional: empty means the hosted Solari API (`DefaultBaseURL`,
  `https://api.getsolari.com`), matching the TypeScript and Python SDKs. An explicit `BaseURL` is used as before.
