// Command atomic-accounting-stress runs a private-local baseline against a
// batch of atomic imports whose source UTXOs have large owner lists.
//
// It intentionally uses only the local AvalancheGo API. It does not contact
// Mainnet, Fuji, or any public RPC endpoint.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/genesis"
	"github.com/ava-labs/avalanchego/graft/coreth/ethclient"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	walletcommon "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
)

type measurement struct {
	mode                 string
	ownerCount           int
	exports              int
	controlConfirmation  time.Duration
	importConfirmation   time.Duration
	legitimateDuringTime time.Duration
	importError          error
	metricDeltas         map[string]float64
}

func main() {
	uri := flag.String("uri", "http://127.0.0.1:9650", "local AvalancheGo API URI")
	mode := flag.String("mode", "attack", "baseline or attack")
	exports := flag.Int("exports", 16, "number of source exports to create before one C import")
	flag.Parse()

	if *mode != "baseline" && *mode != "attack" {
		fatalf("mode must be baseline or attack")
	}
	if *exports < 1 || *exports > 64 {
		fatalf("exports must be between 1 and 64")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	m, err := run(ctx, *uri, *mode, *exports)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Println()
	fmt.Println("Private-network stress result")
	fmt.Printf("mode=%s owners=%d exports=%d control_confirmation=%s import_confirmation=%s legitimate_during_import=%s\n",
		m.mode, m.ownerCount, m.exports, m.controlConfirmation, m.importConfirmation, m.legitimateDuringTime)
	for name, value := range m.metricDeltas {
		fmt.Printf("metric_delta %s=%g\n", name, value)
	}
	if m.importError != nil {
		fatalf("atomic import: %v", m.importError)
	}
	fmt.Println("PASS: local baseline and atomic import completed without public-network traffic")
}

func run(ctx context.Context, uri, mode string, exportCount int) (measurement, error) {
	key := genesis.EWOQKey
	if mode == "attack" {
		// The local tmpnet genesis pre-funds both legacy test keys. Using a
		// different key for the attack case lets baseline and attack UTXOs
		// coexist in one private network while each import selects only its
		// own source outputs.
		key = genesis.VMRQKey
	}
	keychain := secp256k1fx.NewKeychain(key)
	baseWallet, err := primary.MakeWallet(ctx, uri, keychain, keychain, primary.WalletConfig{})
	if err != nil {
		return measurement{}, fmt.Errorf("make wallet: %w", err)
	}

	ownerCount := 1
	if mode == "attack" {
		ownerCount = 2048
	}
	owner := outputOwners(key.Address(), ownerCount)
	xWallet := baseWallet.X()
	cWallet := baseWallet.C()
	xChainID := xWallet.Builder().Context().BlockchainID
	cChainID := cWallet.Builder().Context().BlockchainID
	assetID := xWallet.Builder().Context().AVAXAssetID

	ethClient, err := localEthClient(uri)
	if err != nil {
		return measurement{}, err
	}
	defer ethClient.Close()

	control, err := sendLegitimate(ctx, ethClient, key, 0x11)
	if err != nil {
		return measurement{}, fmt.Errorf("control transaction: %w", err)
	}

	fmt.Printf("mode=%s owners=%d exports=%d\n", mode, ownerCount, exportCount)
	fmt.Printf("control legitimate confirmation: %s\n", control)
	for i := 0; i < exportCount; i++ {
		start := time.Now()
		_, err := xWallet.IssueExportTx(
			cChainID,
			[]*avax.TransferableOutput{{
				Asset: avax.Asset{ID: assetID},
				Out: &secp256k1fx.TransferOutput{
					Amt:          2 * units.Avax,
					OutputOwners: owner,
				},
			}},
			walletcommon.WithMinIssuanceTime(0),
		)
		if err != nil {
			return measurement{}, fmt.Errorf("export %d: %w", i, err)
		}
		fmt.Printf("export %d/%d confirmation: %s\n", i+1, exportCount, time.Since(start))
	}

	// Re-fetch state after all exports. This makes the C import consume the
	// exact set of atomic UTXOs visible through the local node API.
	baseWallet, err = primary.MakeWallet(ctx, uri, keychain, keychain, primary.WalletConfig{})
	if err != nil {
		return measurement{}, fmt.Errorf("refresh wallet: %w", err)
	}
	cWallet = baseWallet.C()
	metricsBefore, err := snapshotMetrics(ctx, uri)
	if err != nil {
		return measurement{}, fmt.Errorf("metrics before import: %w", err)
	}

	importStart := time.Now()
	importDone := make(chan error, 1)
	go func() {
		_, err := cWallet.IssueImportTx(
			xChainID,
			key.PublicKey().EthAddress(),
			walletcommon.WithBaseFee(big.NewInt(1)),
			walletcommon.WithPollFrequency(10*time.Millisecond),
		)
		importDone <- err
	}()

	// Submit an ordinary C-chain transfer shortly after the atomic request.
	// The metric deltas below are the authoritative admission/execution data.
	time.Sleep(20 * time.Millisecond)
	during, err := sendLegitimate(ctx, ethClient, key, 0x22)
	if err != nil {
		return measurement{}, fmt.Errorf("legitimate transaction during import: %w", err)
	}
	importErr := <-importDone
	importDuration := time.Since(importStart)
	metricsAfter, err := snapshotMetrics(ctx, uri)
	if err != nil {
		return measurement{}, fmt.Errorf("metrics after import: %w", err)
	}
	metricDeltas := make(map[string]float64)
	for name, after := range metricsAfter {
		metricDeltas[name] = after - metricsBefore[name]
	}

	return measurement{
		mode:                 mode,
		ownerCount:           ownerCount,
		exports:              exportCount,
		controlConfirmation:  control,
		importConfirmation:   importDuration,
		legitimateDuringTime: during,
		importError:          importErr,
		metricDeltas:         metricDeltas,
	}, nil
}

var metricNames = []string{
	"avalanche_meterchainvm_verify_sum{chain=\"C\"}",
	"avalanche_meterchainvm_verify_count{chain=\"C\"}",
	"avalanche_meterchainvm_accept_sum{chain=\"C\"}",
	"avalanche_meterchainvm_accept_count{chain=\"C\"}",
	"avalanche_meterchainvm_build_block_sum{chain=\"C\"}",
	"avalanche_meterchainvm_build_block_count{chain=\"C\"}",
	"avalanche_evm_eth_atomic_mempool_added_txs{chain=\"C\"}",
	"avalanche_evm_eth_chain_block_executions{chain=\"C\"}",
	"avalanche_evm_eth_chain_block_gas_used_processed{chain=\"C\"}",
}

func snapshotMetrics(ctx context.Context, uri string) (map[string]float64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(uri, "/")+"/ext/metrics", nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("metrics returned HTTP %d", response.StatusCode)
	}
	wanted := make(map[string]struct{}, len(metricNames))
	for _, name := range metricNames {
		wanted[name] = struct{}{}
	}
	result := make(map[string]float64, len(metricNames))
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		for _, name := range metricNames {
			if !strings.HasPrefix(line, name+" ") {
				continue
			}
			value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name)), 64)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", name, err)
			}
			result[name] = value
			delete(wanted, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for name := range wanted {
		result[name] = 0
	}
	return result, nil
}

func outputOwners(signing ids.ShortID, count int) secp256k1fx.OutputOwners {
	owners := make([]ids.ShortID, count)
	base := new(big.Int).SetBytes(signing[:])
	for i := range owners {
		value := new(big.Int).Add(base, big.NewInt(int64(i)))
		encoded := value.Bytes()
		if len(encoded) > len(signing) {
			fatalf("owner counter overflow")
		}
		copy(owners[i][len(signing)-len(encoded):], encoded)
	}
	return secp256k1fx.OutputOwners{Threshold: 1, Addrs: owners}
}

func localEthClient(uri string) (*ethclient.Client, error) {
	address := strings.TrimRight(uri, "/") + "/ext/bc/C/rpc"
	client, err := ethclient.Dial(address)
	if err != nil {
		return nil, fmt.Errorf("dial local C RPC: %w", err)
	}
	return client, nil
}

func sendLegitimate(ctx context.Context, client *ethclient.Client, key *secp256k1.PrivateKey, tag byte) (time.Duration, error) {
	nonce, err := client.AcceptedNonceAt(ctx, key.PublicKey().EthAddress())
	if err != nil {
		return 0, fmt.Errorf("accepted nonce: %w", err)
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return 0, fmt.Errorf("suggest gas price: %w", err)
	}
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return 0, fmt.Errorf("chain id: %w", err)
	}
	to := common.Address{tag}
	tx := types.NewTransaction(nonce, to, big.NewInt(1), 21_000, gasPrice, nil)
	signed, err := types.SignTx(tx, types.NewEIP155Signer(chainID), key.ToECDSA())
	if err != nil {
		return 0, fmt.Errorf("sign legitimate tx: %w", err)
	}
	start := time.Now()
	if err := client.SendTransaction(ctx, signed); err != nil {
		return 0, fmt.Errorf("send legitimate tx: %w", err)
	}
	for {
		_, err := client.TransactionReceipt(ctx, signed.Hash())
		if err == nil {
			return time.Since(start), nil
		}
		if !errors.Is(err, ethereum.NotFound) {
			return 0, fmt.Errorf("receipt: %w", err)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func fatalf(format string, args ...any) {
	panic(fmt.Errorf(format, args...))
}
