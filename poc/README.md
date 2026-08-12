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

## Defined accounting parameters

The source code gives us an exact resource envelope to compare against; it
does not define a separate owner-list or shared-memory "compute unit" price.
For an atomic C-Chain transaction, `gasUsed` is the intrinsic atomic gas plus
one gas per serialized unsigned C transaction byte and 1,000 gas per
signature. `AsOp` uses that value for the operation gas. The transaction pool
admits the operation while its gas is at most `dynamic.MinTarget` (1,000,000).

SAE also defines `TargetBlockBytes` as 1,572,864 bytes (1.5 MiB) and
`MaxBlockBytes` as 1,835,008 bytes (1.75 MiB). The tx-gossip rule connects a
transaction's serialized size `y` and gas limit `g` to the live block gas limit
`x` using `y / TargetBlockBytes <= g / x`. The block builder applies the same
serialized transaction-byte budget to end-of-block atomic operations.

The 2,048-owner case is 307 signed C-Chain bytes and 11,230 gas, so it passes
the 1,000,000-gas pool gate. Its external source UTXO is 41,058 bytes and the
signed source export is 41,275 bytes. Those values are fetched and verified
before the block builder accounts only for the 307-byte C transaction. The
ratio is 133.74x for source-UTXO bytes per C-import byte and 134.45x for the
signed source-export envelope per C-import byte.

For a direct envelope comparison, `floor(1,572,864 / 307) = 5,123` such C
imports fit within the configured target serialized-byte budget. That same
budget would represent 1,572,761 C-import bytes but 210,340,134 source-UTXO
bytes and 211,451,825 signed source-export bytes. This is an accounting
comparison, not a claim that 5,123 imports can all execute in one block; gas,
state conflicts, and the live dynamic block limit remain separate constraints.

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
