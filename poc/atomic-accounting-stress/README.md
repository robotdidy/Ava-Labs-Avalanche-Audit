# Atomic accounting private-network stress runner

`main.go` runs a paired baseline/attack check against a private AvalancheGo
tmpnet. It uses the production primary wallet, X-Chain export path, C-Chain
atomic import path, local atomic mempool, and local metrics endpoint.

The baseline creates one-owner source UTXOs. The attack case creates
2,048-owner source UTXOs with a threshold of one, so only the controlled first
owner needs a signature. Each run creates a batch of exports, imports them in
one C-Chain transaction, and submits an ordinary C-Chain transfer while the
import is pending. Metrics are captured immediately before and after the
import.

Run only against a local tmpnet. Do not point this program at Mainnet, Fuji,
or a public RPC endpoint.

From the AvalancheGo checkout on Linux/WSL2:

```bash
GOTOOLCHAIN=local CGO_ENABLED=1 go run ./cmd/atomic-accounting-stress \
  -uri=http://127.0.0.1:<local-api-port> -mode=baseline -exports=16

GOTOOLCHAIN=local CGO_ENABLED=1 go run ./cmd/atomic-accounting-stress \
  -uri=http://127.0.0.1:<local-api-port> -mode=attack -exports=16
```

The runner is supporting evidence only. Compare paired fresh-network runs and
use the metric deltas, not one wall-clock result, when assessing processing
work. A successful run does not by itself establish a network-wide denial of
service or Immunefi's separate percentage threshold.
