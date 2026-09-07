# testdata

`wire-contract.json` is a copy of `tests/sdk/ts-request-fixtures.json` from the
Solari monorepo — the canonical cross-language wire contract that the reference
(TypeScript) SDK's contract suite regenerates on every run. It is republished
here whenever the contract changes; treat the monorepo fixture as the source of
truth. The human-readable half of the contract is `PROTOCOL.md` at the repo
root.

The test reads it via `testdata/` (not a relative path out of the module)
because `go test` caches results and only invalidates on files opened INSIDE
the module — see the comment on `contractFixturePath` in
`contract_wire_test.go`.
