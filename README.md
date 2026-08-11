# Ava Labs Avalanche audit

This repository contains the AVA-DO-001 AvalancheGo finding and its local
proof of concept.

## Finding

The report covers external source-UTXO payloads used by C-Chain atomic imports.
The C-Chain import and its gas charge remain small while nodes fetch, verify,
and remove a larger source UTXO and its shared-memory trait indexes.

- Report: `reports/AVA-DO-001_External_Atomic_UTXO_Payload_Accounting.md`
- Immunefi submission text: `reports/immunefi-final-submission.md`

## PoC

The primary PoC is the standalone runner in `poc/main.go`. It was validated
against AvalancheGo commit `be3f61b9caa04a1ff6684fffbc553baad757ceb6`.

To run it, copy `poc/main.go` to
`cmd/atomic-accounting-poc/main.go` in the matching AvalancheGo checkout and
run:

```bash
GOTOOLCHAIN=local CGO_ENABLED=1 go run ./cmd/atomic-accounting-poc
```

The full bundle and captured output are in `poc/`. The runner uses a local
in-memory shared-memory database and makes no Mainnet, Fuji, RPC, or peer
network requests.
