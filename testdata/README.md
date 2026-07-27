# testdata

`wire-contract.json` is a SYMLINK to `tests/sdk/ts-request-fixtures.json` — the
canonical cross-language wire contract that `tests/sdk/contract.test.ts`
regenerates from the reference (TypeScript) SDK.

It is a symlink rather than a copy so there is exactly ONE contract in the repo:
a second copy would be a second source of truth, free to drift from the first,
which is the bug class this contract exists to kill.

It is a symlink rather than a `../../tests/sdk/...` path in the test because
`go test` caches results and only invalidates on files opened INSIDE the module.
Reading the fixture directly by relative path left a tampered contract passing
from cache — see the comment on `contractFixturePath` in `contract_wire_test.go`.
