# AvalancheGo external atomic-UTXO accounting PoC

This is a standalone, local-only PoC for the C-Chain atomic-import accounting
issue. It uses AvalancheGo's production transaction, codec, credential
verification, gas, and shared-memory implementations. It does not contact
Mainnet, Fuji, a public RPC endpoint, or a peer network.

The runner creates valid threshold-one X-Chain export outputs with increasing
owner lists, creates the corresponding signed C-Chain imports, and checks that:

- the source UTXO and signed export grow with the owner/trait count;
- the signed C-Chain import remains the same size;
- the charged C-Chain gas remains the same;
- `SanityCheck()` and `VerifyCredentials()` pass for every case; and
- shared-memory removal work grows with the number of traits.

## Source

The runner was validated against AvalancheGo commit
`be3f61b9caa04a1ff6684fffbc553baad757ceb6`.

## Requirements

- Linux or WSL2
- Go 1.25.10 (or the Go version declared by the checkout)
- `CGO_ENABLED=1`
- A complete AvalancheGo checkout with its normal module dependencies

## Run

From the root of the AvalancheGo checkout:

```bash
GOTOOLCHAIN=local CGO_ENABLED=1 go run ./cmd/atomic-accounting-poc
```

The command exits non-zero if a transaction fails validation, the signed
export exceeds the generic 64 KiB limit, or the charged gas changes between
owner-count cases.

The deterministic output is the important result. The timing and allocation
measurements are local comparative measurements and will vary with the host.

## Captured result

The included `poc-output.txt` records one successful run. The deterministic
result was:

| Owners / traits | Source UTXO | Signed source export | Signed C import | Charged gas |
|---:|---:|---:|---:|---:|
| 1 | 118 bytes | 335 bytes | 307 bytes | 11,230 |
| 32 | 738 bytes | 955 bytes | 307 bytes | 11,230 |
| 256 | 5,218 bytes | 5,435 bytes | 307 bytes | 11,230 |
| 1,024 | 20,578 bytes | 20,795 bytes | 307 bytes | 11,230 |
| 2,048 | 41,058 bytes | 41,275 bytes | 307 bytes | 11,230 |

The existing `vms/saevm/cchain/tx/atomic_accounting_benchmark_test.go` is
included as supplemental source-level coverage. This directory's `main.go`
is the standalone PoC runner.
