# AVA-DO-001 - External atomic-UTXO payloads escape C-Chain admission and gas accounting

**Target:** `ava-labs/avalanchego` (SAE C-Chain atomic-import path)
**Recommended severity:** High - *causing network processing nodes to process transactions from the mempool beyond set parameters*
**Secondary classification:** Medium resource griefing (do not submit as a second report)
**Validation status:** Reproduced locally with production code, source-level impact confirmed
**Source snapshot:** `avalanchego` `be3f61b9caa04a1ff6684fffbc553baad757ceb6` (`graft/subnet-evm/v1.14.2-401-gbe3f61b9ca`)

## Summary

A C-Chain atomic `Import` is charged for the serialized destination transaction and its signatures, but each node that admits/processes the transaction must fetch, decode, authenticate, and later delete externally stored source-chain UTXO data. The source UTXO's owner list and shared-memory trait count are not part of the C transaction and are not included in the gas, txpool admission, or block-byte accounting.

A valid threshold-one UTXO with sorted filler owners is therefore a low-cost amplifier. In the focused PoC, a fee-paying import (2,000,000 nAVAX input and 1,000,000 nAVAX destination output) increased the owner list from 1 to 2,048, growing the external UTXO from 118 to 41,058 bytes and the signed source export from 335 to 41,275 bytes. The shared-memory trait count grew from 1 to 2,048, while the signed C import stayed 307 bytes and the charged gas stayed 11,230. Credential verification passed in every case.

This is a High-scope accounting/admission finding, not a theft or consensus finding. The PoC does not claim total network shutdown, a chain split, fund loss, or a measured network-wide percentage increase.

## Scope and severity mapping

The supplied Avalanche DLT scope snapshot (page 6 of `Ava Labs Avalanche Bug Bounties _ Immunefi(SCOPE-blockchain-dlt).pdf`) lists the exact High impact: **"Causing network processing nodes to process transactions from the mempool beyond set parameters."** The same snapshot requires an exploitable path, disproportionate resource use, and a realistic attack scenario (page 3). The source/isolated-node PoC establishes the vulnerable path, the disproportionate work, and the attacker-controlled input:

- the input is a valid signed atomic import, not malformed traffic;
- the attacker needs an ordinary X-Chain signing key and a chosen C-Chain destination address, not a validator key, special peer position, or machine access;
- the external work scales with source UTXO bytes and owner/trait count while the C admission charge does not; and
- the pool and block-hook paths are public transaction paths used by RPC and gossip.

The current PoC does not claim a network-wide load percentage or a measured
prevention of legitimate users. Those are separate impact measurements and
should be demonstrated on a private local network before describing the High
impact as fully demonstrated.

The scope snapshot excludes network-level TCP/P2P DoS and API panic/OOM testing. No such test was performed. All validation below uses an in-memory shared-memory database and no state-changing Mainnet or Fuji request, consistent with [`avalanchego/SECURITY.md`](avalanchego/SECURITY.md).

## Root cause

The C atomic gas formula is:

```text
gas = 10,000 + len(serialized unsigned C transaction) + 1,000 * signature_count
```

`gasUsed` calls `c.Size` on the unsigned C transaction only and adds one gas per destination byte plus the signature charge: [`tx.go`](avalanchego/vms/saevm/cchain/tx/tx.go#L165-L200). `AsOp` uses that result for the operation's gas: [`tx.go`](avalanchego/vms/saevm/cchain/tx/tx.go#L140-L162).

The omitted work is in the import verifier:

1. `verifyCredentials` builds source UTXO IDs and calls `SharedMemory.Get`.
2. It parses the returned external UTXO bytes with `ParseUTXO`.
3. It checks the asset and calls `fx.VerifyTransfer`, which verifies the owner condition and supplied signature index.
4. `OutputOwners.Verify` scans the complete sorted/unique address list but imposes no owner-count bound.

References: [`import.go`](avalanchego/vms/saevm/cchain/tx/import.go#L157-L208), [`output_owners.go`](avalanchego/vms/secp256k1fx/output_owners.go#L113-L125).

The same external work is repeated in the block path. `PotentialEndOfBlockOps` re-runs `VerifyCredentials`, including for transactions already checked by the pool: [`hooks.go`](avalanchego/vms/saevm/cchain/hooks.go#L355-L419). The SAE block builder then applies its byte-budget check to `tx.Size()`, which is the serialized C transaction and not the source UTXO payload: [`block_builder.go`](avalanchego/vms/saevm/sae/block_builder.go#L276-L329), [`hooks.go`](avalanchego/vms/saevm/cchain/hooks.go#L525-L547).

The txpool rejects only when the calculated operation gas exceeds `dynamic.MinTarget` (`1,000,000`) and explicitly reasons that one gas per C transaction byte bounds pool memory. The external bytes are invisible to that check: [`txpool.go`](avalanchego/vms/saevm/cchain/txpool/txpool.go#L177-L225), [`target.go`](avalanchego/vms/saevm/cchain/dynamic/target.go#L16-L25).

On the source side, the owner list is copied into atomic shared-memory traits during export: [`executor.go`](avalanchego/vms/avm/txs/executor/executor.go#L107-L140). Shared memory writes one index entry per trait and removes one index entry per trait on consumption: [`state.go`](avalanchego/chains/atomic/state.go#L75-L118), [`state.go`](avalanchego/chains/atomic/state.go#L148-L176). The ordinary AVM UTXO state similarly indexes every owner address: [`utxo_state.go`](avalanchego/vms/components/avax/utxo_state.go#L172-L229).

## Reproduction

The primary PoC is the standalone runner [main.go](avalanchego/cmd/atomic-accounting-poc/main.go); the benchmark test [atomic_accounting_benchmark_test.go](avalanchego/vms/saevm/cchain/tx/atomic_accounting_benchmark_test.go) is supplemental coverage. Both use the real C-Chain transaction types, `SanityCheck`, `VerifyCredentials`, `AsOp`, `AtomicRequests`, and `chains/atomic` shared-memory implementation. Each synthetic source UTXO is canonical-marshaled, passes `utxo.Verify()`, has one controlled threshold-one owner followed by sorted filler owners, and is inserted into a local `memdb`. The same output is also serialized and signed as an AVM `ExportTx`; the 2,048-owner export is 41,275 bytes and passes the generic 64 KiB size check. The PoC also asserts a nonzero gas-fee cap. No public network is contacted.

Run on Linux/WSL from the `avalanchego` checkout:

```bash
GOTOOLCHAIN=local CGO_ENABLED=1 go run ./cmd/atomic-accounting-poc

go test ./vms/saevm/cchain/tx \
  -run TestAtomicImportExternalPayloadAccounting -count=1 -v

go test ./vms/saevm/cchain/tx -run '^$' \
  -bench BenchmarkAtomicImportExternalPayload -benchtime=1s -benchmem -count=1

go test ./vms/saevm/cchain/tx -run '^$' \
  -bench BenchmarkAtomicImportTraitRemoval -benchtime=250ms -benchmem -count=1
```

The supplied Windows checkout was copied byte-for-byte (excluding `.git`) to a short WSL path because the repository declares Windows unsupported and its cgo/FFI dependencies do not build with the native Windows toolchain. The successful run used Go `1.25.10 linux/amd64`, `CGO_ENABLED=1`, and the production source at the commit above.

### Deterministic accounting result

All five cases passed `SanityCheck` and `VerifyCredentials`; gas is identical despite the external payload growing by 347.95x.

| owners / traits | source UTXO bytes | signed source export bytes | signed C import bytes | charged gas |
|---:|---:|---:|---:|---:|
| 1 | 118 | 335 | 307 | 11,230 |
| 32 | 738 | 955 | 307 | 11,230 |
| 256 | 5,218 | 5,435 | 307 | 11,230 |
| 1,024 | 20,578 | 20,795 | 307 | 11,230 |
| 2,048 | 41,058 | 41,275 | 307 | 11,230 |

The 2,048-owner source value and the complete signed source export are both below the generic [64 KiB transaction-size ceiling](avalanchego/vms/txs/mempool/mempool.go#L23-L26). The filler owners do not need keys; the first owner supplies the one signature required by the threshold-one condition.

### Defined accounting parameters

The source code defines an exact gas/byte envelope, but it does not assign a
separate compute-unit price to each external owner or shared-memory trait.
`gasUsed` charges the atomic intrinsic amount, one gas per serialized unsigned
C-Chain transaction byte, and 1,000 gas per signature; `AsOp` uses that result
as the operation gas ([`tx.go`](avalanchego/vms/saevm/cchain/tx/tx.go#L140-L200)).
The txpool admits an operation while its gas is at most
`dynamic.MinTarget = 1,000,000` ([`txpool.go`](avalanchego/vms/saevm/cchain/txpool/txpool.go#L177-L225),
[`target.go`](avalanchego/vms/saevm/cchain/dynamic/target.go#L16-L25)).

SAE's exact serialized-byte parameters are `TargetBlockBytes = 1,572,864`
(1.5 MiB) and `MaxBlockBytes = 1,835,008` (1.75 MiB)
([`params.go`](avalanchego/vms/saevm/params/params.go#L34-L60)). The tx-gossip
admission rule for ordinary EVM transactions is `y / TargetBlockBytes <= g / x`,
where `y` is serialized transaction size, `g` is its gas limit, and `x` is the
live block gas limit ([`txgossip.go`](avalanchego/vms/saevm/txgossip/txgossip.go#L125-L181)).
Atomic imports use the separate C-Chain txpool gate above, but the SAE block
builder applies the same serialized-byte budget to their end-of-block
operations ([`block_builder.go`](avalanchego/vms/saevm/sae/block_builder.go#L276-L329)).
The active block gas capacity is dynamic: `SafeMaxBlockSize(rate) = rate *
TauSeconds * Lambda`, with `TauSeconds = 5` and `Lambda = 2`; at the 4,000,000
gas target used by the current SAE tests and launch documentation this is an
80,000,000-gas block limit ([`state.go`](avalanchego/vms/saevm/worstcase/state.go#L89-L158),
[`vm_test.go`](avalanchego/vms/saevm/sae/vm_test.go#L138-L145)). The old
`ap5.AtomicGasLimit = 100,000` belongs to the legacy Coreth atomic path; current
SAE puts these operations in `Header.GasUsed` rather than the retired
`ExtDataGasUsed` field ([`hooks.go`](avalanchego/vms/saevm/cchain/hooks.go#L395-L410)).

The 2,048-owner case is 307 signed C-Chain bytes and 11,230 gas, so it is far
below the 1,000,000-gas pool gate. Its external source UTXO is 41,058 bytes and
the signed source export is 41,275 bytes. The import verifier fetches and
processes that external data before the block builder accounts only for the
307-byte C transaction ([`import.go`](avalanchego/vms/saevm/cchain/tx/import.go#L157-L208),
[`hooks.go`](avalanchego/vms/saevm/cchain/hooks.go#L355-L419)). The external
source UTXO is therefore 133.74x the C-import size, and the signed source
export is 134.45x the C-import size, with no change to charged gas.

As a direct envelope comparison, `floor(1,572,864 / 307) = 5,123` such C
imports fit within the configured target serialized-byte budget. That same
budget would represent 1,572,761 C-import bytes but 210,340,134 source-UTXO
bytes and 211,451,825 signed source-export bytes. This is a deterministic
accounting comparison, not a claim that 5,123 imports can all execute in one
block; gas, state conflicts, and the live dynamic block limit remain separate
constraints.

The block gas capacity is dynamic rather than a fixed owner-compute budget.
`SafeMaxBlockSize(rate)` multiplies the current gas rate by
`TauSeconds * Lambda = 5 * 2 = 10`; at the 4,000,000-gas target used by the
current SAE test configuration, the corresponding rate is 8,000,000 and the
block capacity is 80,000,000 gas ([`state.go`](avalanchego/vms/saevm/worstcase/state.go#L89-L158),
[`vm_test.go`](avalanchego/vms/saevm/sae/vm_test.go#L138-L145)). The C-Chain
dedicated pool is constructed with a maximum of 1,024 transactions
([`vm.go`](avalanchego/vms/saevm/cchain/vm.go#L160-L170)). If that pool held
1,024 distinct 2,048-owner imports, the C descriptors would total 314,368
bytes and 11,499,520 gas, while the external source UTXOs would total
42,043,392 bytes and 2,097,152 trait entries. These are bounded-capacity
arithmetic figures, not a claim that a public network was filled or that a
network-wide percentage was measured.

### Verification benchmark

`BenchmarkAtomicImportExternalPayload` repeatedly executes the same `VerifyCredentials` path that txpool admission and the C block hook execute. Results are from an Intel i5-10210U under WSL and are comparative, not a network-throughput claim.

| owners | ns/op | bytes/op | allocs/op |
|---:|---:|---:|---:|
| 1 | 14,474 | 13,637 | 179 |
| 32 | 24,276 | 18,781 | 274 |
| 256 | 104,474 | 69,085 | 952 |
| 1,024 | 349,184 | 261,474 | 3,261 |
| 2,048 | 719,404 | 523,646 | 6,336 |

From 1 to 2,048 owners, verification grows by approximately 49.7x in wall time and 38.4x in allocated bytes in the captured run, while the charged gas remains unchanged. Absolute timings vary with host load; the fixed-gas/external-size relationship and monotonic allocation growth are the reproducible observations.

### Acceptance/removal benchmark

`BenchmarkAtomicImportTraitRemoval` stops the timer while creating the sample and measures the real shared-memory `Apply` call that removes the imported value and deletes one trait index per owner.

| owners | ns/op | bytes/op | allocs/op |
|---:|---:|---:|---:|
| 1 | 54,450 | 146,559 | 480 |
| 32 | 355,980 | 425,494 | 3,802 |
| 256 | 2,033,480 | 2,452,654 | 27,777 |
| 1,024 | 7,763,920 | 9,714,640 | 109,973 |
| 2,048 | 14,899,810 | 19,666,206 | 219,567 |

The measured removal path grows by approximately 274x in time and 134x in allocated bytes from 1 to 2,048 traits. This is acceptance-side evidence of the same unpriced trait dimension; it is not presented as a network-wide denial-of-service measurement.

### Private-network stress validation

The standalone stress runner was then exercised against a private four-validator
AvalancheGo tmpnet using only its local API. It ran a paired baseline and attack
case on the same network. The baseline used 16 exports with one owner each; the
attack used 16 exports with 2,048 owners each. The baseline and attack used
different pre-funded local keys so their UTXOs did not overlap. Both cases
completed the production X-Chain export, C-Chain atomic import, atomic mempool,
and block-processing paths.

The captured run recorded the following values:

| case | import confirmation | ordinary transfer submitted during import | C-Chain VM verify sum | atomic mempool additions |
|---|---:|---:|---:|---:|
| baseline, 1 owner | 2.122029674 s | 2.092251873 s | 8.1705 ms | 1 |
| attack, 2,048 owners | 2.227012650 s | 269.846718 ms | 41.542203 ms | 1 |

The metric counts differed between the two runs, and acceptance timing includes
consensus and scheduling work. The aggregate values are therefore not used as
an isolated per-transaction CPU multiplier. In particular, the ordinary
transfer was not delayed in the attack run. The stress run confirms live local
reachability and owner-dependent work in the production path; it does not
establish a reproducible network-wide delay, loss of access, or the program's
separate 30-percent node-resource threshold. The complete capture is in
[`AVA-DO-001_private_stress_results.txt`](AVA-DO-001_private_stress_results.txt).

## Attacker path and impact

1. An ordinary X-Chain user creates a valid transfer/export output with a threshold of one, their own address, and many sorted filler addresses. `OutputOwners.Verify` accepts the list because it checks threshold and sorted uniqueness but has no owner-count limit.
2. X-Chain export serializes the complete UTXO and copies every owner address into the atomic element's traits.
3. The attacker submits a normal signed C-Chain import containing only source UTXO IDs, signature indices, and the one required signature. The C transaction remains small.
4. The C txpool fetches and verifies the external values. Receiving peers repeat the same work when they admit/gossip the transaction. The block hook re-verifies it before the builder's destination-byte check. Acceptance removes one shared-memory index entry per trait.

The direct impact is disproportionate processing relative to the C transaction's gas, txpool `MinTarget`, and block-byte budget. Multiple distinct source UTXOs can be referenced by one import, so the mismatch is additive; this report deliberately does not assert a maximum number of inputs or a network-wide load percentage because the private stress run did not isolate those limits.

This is not **Modification of transaction fees outside of design parameters**.
The gas and fee for the C-Chain transaction are calculated by the existing
formula; the defect is that external UTXO bytes, owner checks, and trait-index
work are omitted from that formula. Selecting the fee-modification impact
would describe a different bug.

No direct theft, unauthorized mint, fund freeze, insolvency, validator-key exposure, consensus violation, or chain split was observed. The source UTXO remains authenticated and is consumed normally; the defect is the unpriced work required to process it.

## Novelty and duplicate check

The supplied audit index lists one AvalancheGo audit, the March 2023 OpenZeppelin MerkleDB audit. Its only `atomic` occurrence is a generic statement about database atomicity/consistency; it does not discuss C/X cross-chain imports, UTXOs, shared-memory traits, or this accounting boundary. Targeted searches of the supplied audit index, source documentation, release notes, and repository history found no disclosure of this external-payload issue.

There is a related but distinct public change: AvalancheGo issue [#5468](https://github.com/ava-labs/avalanchego/issues/5468) and PR [#5549](https://github.com/ava-labs/avalanchego/pull/5549). That work addresses the serialized **destination atomic transaction** and its block `ExtData` footprint: its stated solution prices `Tx.AsOp` by serialized bytes and adds a serialized-byte backstop in the block builder. The current code and the PR discussion do not price source-chain UTXO values, owner scans, shared-memory reads, or trait deletion. This report is not the generic "raise gas per cross-chain byte" TODO at [`txpool.go`](avalanchego/vms/saevm/cchain/txpool/txpool.go#L208-L209); it is the separate external-data boundary that remains after that change.

The v1.13.5 release note's generic "C-Chain atomic mempool edge cases" wording was also checked. No corresponding external-payload accounting fix is present in the current source history. The known `Diff.Apply` ordering issue listed in the supplied scope is unrelated.

## Recommended remediation

1. Define a checked atomic-import complexity budget that includes the sum of fetched source UTXO bytes, owner/trait count, decode cost, signature verification, and shared-memory index work. Reject over-budget imports before pool insertion.
2. Enforce the same budget in C/P parsing, txpool admission, gossip, block-hook verification, block building, and shared-memory application. Do not let one stage account only for destination bytes.
3. Add consensus-safe limits on exported UTXO size and `OutputOwners.Addrs` length. Enforce them at source creation and destination import.
4. Price or cap X-Chain owner-address indexes and atomic trait indexes; a fixed ordinary transaction fee must not be the only cost for thousands of index entries.
5. Add regression tests varying only external UTXO bytes and trait count and assert that calculated gas/complexity rises with both dimensions. Add a pool/block-builder test proving that a byte-budget rejection cannot trigger unbudgeted external verification.

## Limitations and maintainer follow-up

- The deterministic PoC and the private tmpnet stress run establish production-path reachability and the reproducible local cost curve, but they do not establish the program's separate `>=30%` network-resource threshold or a consistent delay to legitimate transactions.
- The source export is signed with the production AVM codec and its owner output passes verification, but the synthetic source UTXO was not executed through a full X-Chain state transition. The measured 41,275-byte signed envelope is below the generic 64 KiB limit; maintainers can run the same owner list through a private X-Chain VM as an additional end-to-end check.
- The supplied read-only Mainnet snapshot recorded `avalanchego/1.14.2` and live X-Chain `txFee = 0.001 AVAX`, while the source checkout is commit `be3f61b9ca`. The live node reported a different build commit (`d6a342ba...`); maintainers should match the exact deployed release artifact before assigning final bounty severity.
- No Mainnet or Fuji transaction was sent. Any further validation should use a private local network only, in accordance with `SECURITY.md`.

**Submission recommendation:** the code-path finding is reproducible and the
private network confirms that the external payload is reached by production
import/mempool processing. The current stress capture does not show a consistent
legitimate-transaction delay or the program's separate `>=30%` resource
threshold, so it is not sufficient by itself to make the High impact
un-rejectable. Do not select the Low fee-modification category: it does not
match the observed behavior. If submitting, attach the standalone runner,
private stress capture, and supplemental benchmark test; explicitly distinguish
issue/PR #5468/#5549; and do not claim a Critical impact or a network-wide
percentage that has not been measured.
