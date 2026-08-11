1. ## Title
C-Chain atomic imports ignore external UTXO size, allowing small mempool transactions to make nodes process far more data than they pay for

2. ## Description

## Brief/Intro
AvalancheGo’s C-Chain atomic imports are charged based on the serialized C-Chain transaction and its signatures, but nodes also fetch and verify the source-chain UTXO stored in shared memory. That UTXO can contain a large owner list whose size and processing cost are not included in the import’s gas or mempool accounting. As a result, a small, valid, fee-paying import can make nodes process and later remove a much larger external payload than the transaction limits account for, potentially reducing processing capacity for other transactions.

## Vulnerability Details
I traced the issue to the way AvalancheGo accounts for C-Chain atomic imports.

A C-Chain import contains references to source-chain UTXOs, destination outputs, and signatures. It does not contain the serialized source UTXO values themselves. The gas calculation only considers the serialized C-Chain transaction and the number of signatures:

```text
gas =
    intrinsic gas
    + serialized unsigned C-Chain transaction size
    + (signature count × 1,000)
```

This is calculated in `vms/saevm/cchain/tx/tx.go` by `gasUsed()` and then assigned to the operation by `AsOp()`.

The source UTXO data is fetched separately during credential verification. In `vms/saevm/cchain/tx/import.go`, `verifyCredentials()`:

1. Builds the source UTXO IDs from the import inputs.
2. Reads the corresponding UTXO bytes from shared memory.
3. Parses each UTXO.
4. Checks that the asset matches the import input.
5. Calls `fx.VerifyTransfer()` to verify the ownership condition and supplied signatures.

The size and complexity of this external data are not included in the C-Chain transaction size or gas calculation.

The source UTXO can contain an `OutputOwners` object with a large address list. `OutputOwners.Verify()` checks the threshold and that the addresses are sorted and unique, but it does not impose a maximum number of owner addresses. A threshold-one output therefore only needs one valid signature, while additional sorted addresses can make the UTXO and its owner list much larger.

When the source export is created, AvalancheGo copies every owner address into the atomic shared-memory element’s `Traits` field:

```go
if out, ok := utxo.Out.(avax.Addressable); ok {
    elem.Traits = out.Addresses()
}
```

The shared-memory implementation then creates one index entry for every trait in `SetValue()`. When the imported UTXO is consumed, `RemoveValue()` loops over the same trait list and deletes each index entry. This means that both the verification path and the later acceptance/removal path grow with the number of owner addresses, even though the C-Chain import remains the same size.

The transaction pool does not account for this work. In `vms/saevm/cchain/txpool/txpool.go`, admission compares the operation gas against `dynamic.MinTarget`. The comments explain that this is intended to keep admitted transactions within the transaction-size and pool-memory limits. However, the operation gas only reflects the C-Chain transaction bytes and signatures. The external UTXO bytes, owner-list scan, shared-memory reads, and trait-index operations are outside that calculation.

The block-building path has the same gap. `PotentialEndOfBlockOps()` verifies the import credentials again, including the external UTXO lookup and ownership checks. Only after that does the block builder apply its byte budget using `tx.Size()`, which measures the serialized C-Chain transaction rather than the source UTXO payload stored in shared memory.

I reproduced this behavior with the production AvalancheGo code in commit:

```text
be3f61b9caa04a1ff6684fffbc553baad757ceb6
```

The test creates valid threshold-one source UTXOs with the same asset and amounts while varying only the number of owner addresses. The first owner controls the required signature; the remaining addresses are sorted filler addresses. Every case passed `SanityCheck()` and `VerifyCredentials()`.

| Owners / traits | Source UTXO | Signed source export | Signed C import | Charged gas |
|---:|---:|---:|---:|---:|
| 1 | 118 bytes | 335 bytes | 307 bytes | 11,230 |
| 32 | 738 bytes | 955 bytes | 307 bytes | 11,230 |
| 256 | 5,218 bytes | 5,435 bytes | 307 bytes | 11,230 |
| 1,024 | 20,578 bytes | 20,795 bytes | 307 bytes | 11,230 |
| 2,048 | 41,058 bytes | 41,275 bytes | 307 bytes | 11,230 |

The largest signed source export remains below the generic 64 KiB transaction-size limit, and the C-Chain import stays at 307 bytes in every case. The external source UTXO grows from 118 bytes to 41,058 bytes, while the gas remains fixed at 11,230.

A representative benchmark of the same `VerifyCredentials()` path used during transaction-pool admission and block processing increased from approximately 33.8 microseconds and 13.6 KB of allocations for one owner to approximately 1.94 milliseconds and 523.6 KB of allocations for 2,048 owners. The exact wall-clock timings vary with the host, but the fixed gas and increasing external work are reproducible.

The shared-memory removal benchmark showed the same pattern. Removing the one-trait value took approximately 139 microseconds and allocated 146 KB, while removing the 2,048-trait value took approximately 35.4 milliseconds and allocated 19.6 MB.

The important mismatch is therefore:

```text
C-Chain charge:
    small serialized import + signatures

Actual node work:
    fetch and decode external UTXO
    scan the complete owner list
    verify the transfer
    read and update shared-memory indexes
    remove one trait entry per owner
```

The import is valid and fee-paying, but the work caused by the external source UTXO is not reflected in its gas, transaction-pool admission cost, or C-Chain block-byte accounting. Multiple source UTXOs can be referenced by an import, so this unpriced work can also accumulate across the inputs of one transaction.

The PoC was run against a local in-memory shared-memory database and did not send transactions to Mainnet or Fuji.

## Impact Details

The impact is resource abuse against AvalancheGo processing nodes. The attacker does not need validator access, special network access, or control of a node. An ordinary X-Chain user can create a valid threshold-one export with a large sorted owner list, then submit a normal C-Chain import using only one valid signature.

In the PoC, the attacker’s source export contains 2,048 owner addresses and is 41,275 bytes, which is below the generic 64 KiB transaction limit. The corresponding C-Chain import is only 307 bytes and is charged the same 11,230 gas as the one-owner case.

When a node admits and processes that import, it must:

- Read the external UTXO from shared memory.
- Decode the full UTXO.
- Scan and validate the complete owner list.
- Verify the transfer and supplied signature.
- Re-check the same external data during block processing.
- Remove one shared-memory trait index for every owner address after acceptance.

None of this additional work is included in the C-Chain import’s gas calculation, mempool admission limit, or block-byte budget.

The measured processing cost increases substantially as the owner list grows:

| Owners | Verification time | Allocated memory |
|---:|---:|---:|
| 1 | 33,763 ns/op | 13,636 bytes/op |
| 2,048 | 1,943,885 ns/op | 523,624 bytes/op |

The shared-memory removal path also increases from 139,026 ns/op and 146,583 bytes for one trait to 35,358,760 ns/op and 19,635,350 bytes for 2,048 traits.

An attacker can submit multiple valid imports, and one import can reference multiple source UTXOs. The unpriced external work therefore compounds across transactions and inputs while each C-Chain transaction continues to appear small and affordable to the mempool.

This consumes CPU time, heap allocations, shared-memory reads, database operations, and block-building capacity on every node that admits or processes the transactions. Those resources are taken from the same processing paths used by legitimate transactions, allowing low-cost attacker transactions to delay normal transaction handling and make nodes process work beyond the parameters used to admit and budget the transactions.

There is no direct theft of user funds in this finding. The affected asset is network processing capacity, and the selected impact is:

**Causing network processing nodes to process transactions from the mempool beyond set parameters.**

## References
Primary code references from the tested AvalancheGo commit:

- [AvalancheGo source commit](https://github.com/ava-labs/avalanchego/tree/be3f61b9caa04a1ff6684fffbc553baad757ceb6)
- [C-Chain atomic gas calculation and `AsOp`](https://github.com/ava-labs/avalanchego/blob/be3f61b9caa04a1ff6684fffbc553baad757ceb6/vms/saevm/cchain/tx/tx.go#L140-L200)
- [External UTXO retrieval and credential verification](https://github.com/ava-labs/avalanchego/blob/be3f61b9caa04a1ff6684fffbc553baad757ceb6/vms/saevm/cchain/tx/import.go#L157-L208)
- [C-Chain transaction-pool admission and gas limit](https://github.com/ava-labs/avalanchego/blob/be3f61b9caa04a1ff6684fffbc553baad757ceb6/vms/saevm/cchain/txpool/txpool.go#L177-L225)
- [Block-hook credential re-verification](https://github.com/ava-labs/avalanchego/blob/be3f61b9caa04a1ff6684fffbc553baad757ceb6/vms/saevm/cchain/hooks.go#L355-L419)
- [Block-builder byte accounting](https://github.com/ava-labs/avalanchego/blob/be3f61b9caa04a1ff6684fffbc553baad757ceb6/vms/saevm/sae/block_builder.go#L276-L329)
- [Owner-list validation](https://github.com/ava-labs/avalanchego/blob/be3f61b9caa04a1ff6684fffbc553baad757ceb6/vms/secp256k1fx/output_owners.go#L113-L125)
- [Exported owner addresses copied into atomic traits](https://github.com/ava-labs/avalanchego/blob/be3f61b9caa04a1ff6684fffbc553baad757ceb6/vms/avm/txs/executor/executor.go#L107-L140)
- [Shared-memory trait insertion and removal](https://github.com/ava-labs/avalanchego/blob/be3f61b9caa04a1ff6684fffbc553baad757ceb6/chains/atomic/state.go#L75-L118)

Documentation and scope:

- [Ava Labs Avalanche Immunefi scope](https://immunefi.com/bug-bounty/avalanche/scope/)
- [Avalanche transaction-fee documentation](https://build.avax.network/docs/rpcs/other/guides/txn-fees)
- [Avalanche X-Chain RPC documentation](https://build.avax.network/docs/rpcs/x-chain)

PoC attachment:

- `AVA-DO-001_PoC_v2.zip`
- `main.go` (primary standalone runner)
- `README.md`
- `poc-output.txt`
- `atomic_accounting_benchmark_test.go` (supplemental benchmark)

Related public context reviewed:

- [AvalancheGo issue #5468](https://github.com/ava-labs/avalanchego/issues/5468)
- [AvalancheGo PR #5549](https://github.com/ava-labs/avalanchego/pull/5549)


3. Proof of Concept

## Proof of Concept

The attached PoC is a standalone command-line program that uses AvalancheGo’s production transaction, codec, credential-verification, gas-calculation, and shared-memory implementations. It does not use mocked validation logic and does not contact Mainnet, Fuji, or any public RPC endpoint.

The PoC:

1. Creates valid source UTXOs with a threshold of one and owner lists containing 1, 32, 256, 1,024, and 2,048 addresses.
2. Uses one private key as the required signer and adds sorted filler addresses to increase the external UTXO size.
3. Serializes and signs each AVM `ExportTx`.
4. Creates a signed C-Chain `ImportTx` that spends 2,000,000 nAVAX and sends 1,000,000 nAVAX to the destination.
5. Stores the source UTXO in a local in-memory shared-memory database.
6. Runs `SanityCheck()`, `VerifyCredentials()`, `AsOp()`, `AtomicRequests()`, and the shared-memory removal path.
7. Compares the external UTXO size and processing work with the C-Chain import size and charged gas.

The runner was tested against AvalancheGo commit:

```text
be3f61b9caa04a1ff6684fffbc553baad757ceb6
```

Run it from the AvalancheGo checkout on Linux or WSL2:

```bash
GOTOOLCHAIN=local CGO_ENABLED=1 go run ./cmd/atomic-accounting-poc
```

The deterministic result is:

| Owners / traits | Source UTXO | Signed source export | Signed C import | Charged gas |
|---:|---:|---:|---:|---:|
| 1 | 118 bytes | 335 bytes | 307 bytes | 11,230 |
| 32 | 738 bytes | 955 bytes | 307 bytes | 11,230 |
| 256 | 5,218 bytes | 5,435 bytes | 307 bytes | 11,230 |
| 1,024 | 20,578 bytes | 20,795 bytes | 307 bytes | 11,230 |
| 2,048 | 41,058 bytes | 41,275 bytes | 307 bytes | 11,230 |

Every case passes `SanityCheck()` and `VerifyCredentials()`. The 2,048-owner export remains below the generic 64 KiB transaction-size limit, while the C-Chain import remains 307 bytes and its charged gas remains 11,230.

The captured benchmark run reported:

| Owners | Verification time | Verification allocations | Trait-removal time | Trait-removal allocations |
|---:|---:|---:|---:|---:|
| 1 | 39.052 µs | 13,637 bytes | 84.28 µs | 146,554 bytes |
| 2,048 | 1.751879 ms | 523,647 bytes | 36.221831 ms | 19,658,402 bytes |

The deterministic size and gas values are unchanged across runs. The benchmark shows that external verification and shared-memory trait removal grow with the owner and trait count even though the C-Chain transaction size and gas charge remain constant.

Attached PoC bundle:

```text
AVA-DO-001_PoC_v2.zip
```

The bundle contains:

```text
main.go
README.md
poc-output.txt
atomic_accounting_benchmark_test.go
```

`main.go` is the primary standalone PoC. The benchmark test is included as supplemental coverage.
