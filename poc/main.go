// Command atomic-accounting-poc builds valid AVM exports and C-Chain imports
// and measures the external source-UTXO work that is not included in the
// C-Chain import's size or gas calculation.
package main

import (
	"fmt"
	"math/big"
	"os"
	"runtime"
	"time"

	"github.com/ava-labs/libevm/common"

	chainsatomic "github.com/ava-labs/avalanchego/chains/atomic"
	"github.com/ava-labs/avalanchego/database/memdb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/vms/avm/fxs"
	avmtxs "github.com/ava-labs/avalanchego/vms/avm/txs"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/saevm/cchain/dynamic"
	cctxtx "github.com/ava-labs/avalanchego/vms/saevm/cchain/tx"
	saeparams "github.com/ava-labs/avalanchego/vms/saevm/params"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
)

var (
	accountingAssetID  = ids.FromStringOrPanic("FvwEAhmxKfeiG8SnEvq42hc6whRyY3EFYAvebMqDNDGCgxN5Z")
	accountingCChainID = ids.FromStringOrPanic("2q9e4r6Mu3U68nU1fYjgbR6JvwrRx36CohpAX5UQxse55x1Q5")
	accountingXChainID = ids.FromStringOrPanic("2oYMBNV4eNHyqk2fjjV5nVQLDbtmNJzq5s3qs3Lo6ftnC6FByM")
)

const (
	sourceInputAmount = uint64(3_000_000)
	inputAmount       = uint64(2_000_000)
	outputAmount      = uint64(1_000_000)
	maxExport         = 64 * 1024
)

type accountingCase struct {
	tx                  *cctxtx.Tx
	destination         chainsatomic.SharedMemory
	utxoBytes           []byte
	exportBytes         []byte
	unsignedImportBytes int
	importBytes         []byte
	gas                 uint64
	traits              int
}

type removalSample struct {
	destination chainsatomic.SharedMemory
	chainID     ids.ID
	requests    *chainsatomic.Requests
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "POC FAILED: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("AvalancheGo external atomic-UTXO accounting PoC")
	fmt.Println("source: production transaction, codec, credential, and shared-memory paths")
	fmt.Println()

	ownerCounts := []int{1, 32, 256, 1024, 2048}
	var baselineGas uint64

	fmt.Println("Deterministic accounting result")
	fmt.Println("owners/traits\tsource UTXO\tsigned export\tsigned C import\tcharged gas")
	for i, ownerCount := range ownerCounts {
		c, err := newAccountingCase(ownerCount)
		if err != nil {
			return fmt.Errorf("owners=%d: %w", ownerCount, err)
		}
		if err := c.tx.VerifyCredentials(c.destination); err != nil {
			return fmt.Errorf("owners=%d VerifyCredentials: %w", ownerCount, err)
		}
		if i == 0 {
			baselineGas = c.gas
		} else if c.gas != baselineGas {
			return fmt.Errorf("charged gas changed from %d to %d", baselineGas, c.gas)
		}
		fmt.Printf("%d\t\t%d\t\t%d\t\t%d\t\t%d\n", c.traits, len(c.utxoBytes), len(c.exportBytes), len(c.importBytes), c.gas)
	}

	// Compare the largest deterministic case with the exact SAE accounting
	// parameters used by txpool admission and block construction. This is a
	// byte/gas envelope comparison, not a claim that the calculated number of
	// imports can all be included in one block despite gas and state conflicts.
	largest, err := newAccountingCase(2048)
	if err != nil {
		return fmt.Errorf("parameter comparison setup: %w", err)
	}
	importsAtTargetBytes := saeparams.TargetBlockBytes / uint64(len(largest.importBytes))
	externalUTXOBytes := importsAtTargetBytes * uint64(len(largest.utxoBytes))
	externalExportBytes := importsAtTargetBytes * uint64(len(largest.exportBytes))
	cImportBytes := importsAtTargetBytes * uint64(len(largest.importBytes))
	fmt.Println()
	fmt.Println("Configured SAE accounting parameters")
	fmt.Printf("txpool MinTarget gas/sec: %d\n", dynamic.MinTarget)
	fmt.Printf("TargetBlockBytes: %d\n", saeparams.TargetBlockBytes)
	fmt.Printf("MaxBlockBytes: %d\n", saeparams.MaxBlockBytes)
	fmt.Printf("2048-owner import: %d signed bytes (%d unsigned bytes), %d gas = 10000 intrinsic + %d bytes + 1000 signature, %d external UTXO bytes, %d signed export bytes\n", len(largest.importBytes), largest.unsignedImportBytes, largest.gas, largest.unsignedImportBytes, len(largest.utxoBytes), len(largest.exportBytes))
	fmt.Printf("floor(TargetBlockBytes / import bytes): %d imports\n", importsAtTargetBytes)
	fmt.Printf("same target-byte envelope: %d C import bytes, %d external UTXO bytes, %d signed export bytes\n", cImportBytes, externalUTXOBytes, externalExportBytes)

	fmt.Println()
	fmt.Println("Verification benchmark (100 measured repetitions per case)")
	fmt.Println("owners/traits\taverage time\taverage allocated bytes")
	for _, ownerCount := range ownerCounts {
		c, err := newAccountingCase(ownerCount)
		if err != nil {
			return fmt.Errorf("verification setup owners=%d: %w", ownerCount, err)
		}
		averageTime, averageBytes, err := measureVerification(c, 100)
		if err != nil {
			return fmt.Errorf("verification owners=%d: %w", ownerCount, err)
		}
		fmt.Printf("%d\t\t%s\t\t%d\n", ownerCount, averageTime, averageBytes)
	}

	fmt.Println()
	fmt.Println("Shared-memory trait-removal benchmark (10 measured repetitions per case)")
	fmt.Println("owners/traits\taverage time\taverage allocated bytes")
	for _, ownerCount := range ownerCounts {
		averageTime, averageBytes, err := measureRemoval(ownerCount, 10)
		if err != nil {
			return fmt.Errorf("removal owners=%d: %w", ownerCount, err)
		}
		fmt.Printf("%d\t\t%s\t\t%d\n", ownerCount, averageTime, averageBytes)
	}

	fmt.Println()
	fmt.Println("PASS: every case passed SanityCheck and VerifyCredentials; charged gas stayed constant while external UTXO and trait work increased.")
	return nil
}

// accountingOwners returns a sorted, unique owner list whose first address is
// the signing address. The threshold remains one for every case, so only the
// first owner needs a private key while all addresses are serialized and
// checked by OutputOwners.Verify.
func accountingOwners(addr ids.ShortID, count int) []ids.ShortID {
	owners := make([]ids.ShortID, count)
	base := new(big.Int).SetBytes(addr[:])
	for i := range owners {
		value := new(big.Int).Add(base, big.NewInt(int64(i)))
		encoded := value.Bytes()
		if len(encoded) > len(addr) {
			panic("owner address counter overflow")
		}
		copy(owners[i][len(addr)-len(encoded):], encoded)
	}
	return owners
}

func newAccountingCase(ownerCount int) (accountingCase, error) {
	sk, err := secp256k1.ToPrivateKey(common.FromHex("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"))
	if err != nil {
		return accountingCase{}, fmt.Errorf("private key: %w", err)
	}

	owners := accountingOwners(sk.Address(), ownerCount)
	// The export spends 3,000,000 nAVAX and creates the 2,000,000 nAVAX UTXO
	// imported by C-Chain. The difference models the X-Chain transaction fee.
	exportInputID := avax.UTXOID{TxID: ids.GenerateTestID()}
	utxo := &avax.UTXO{
		Asset: avax.Asset{ID: accountingAssetID},
		Out: &secp256k1fx.TransferOutput{
			Amt: inputAmount,
			OutputOwners: secp256k1fx.OutputOwners{
				Threshold: 1,
				Addrs:     owners,
			},
		},
	}
	exportInput := &avax.TransferableInput{
		UTXOID: exportInputID,
		Asset:  avax.Asset{ID: accountingAssetID},
		In: &secp256k1fx.TransferInput{
			Amt: sourceInputAmount,
			Input: secp256k1fx.Input{
				SigIndices: []uint32{0},
			},
		},
	}

	exportTx := &avmtxs.ExportTx{
		BaseTx: avmtxs.BaseTx{BaseTx: avax.BaseTx{
			NetworkID:    constants.MainnetID,
			BlockchainID: accountingXChainID,
			Ins:          []*avax.TransferableInput{exportInput},
		}},
		DestinationChain: accountingCChainID,
		ExportedOuts: []*avax.TransferableOutput{{
			Asset: avax.Asset{ID: accountingAssetID},
			FxID:  secp256k1fx.ID,
			Out:   utxo.Out.(avax.TransferableOut),
		}},
	}
	parser, err := avmtxs.NewParser([]fxs.Fx{&secp256k1fx.Fx{}})
	if err != nil {
		return accountingCase{}, fmt.Errorf("AVM parser: %w", err)
	}
	unsignedExportBytes, err := parser.Codec().Marshal(avmtxs.CodecVersion, exportTx)
	if err != nil {
		return accountingCase{}, fmt.Errorf("marshal export: %w", err)
	}
	signedExport := &avmtxs.Tx{Unsigned: exportTx}
	if err := signedExport.SignSECP256K1Fx(parser.Codec(), [][]*secp256k1.PrivateKey{{sk}}); err != nil {
		return accountingCase{}, fmt.Errorf("sign export: %w", err)
	}
	exportBytes := signedExport.Bytes()
	if len(exportBytes) <= len(unsignedExportBytes) || len(exportBytes) >= maxExport {
		return accountingCase{}, fmt.Errorf("signed export size %d is outside expected range", len(exportBytes))
	}

	// AVM assigns exported outputs after the regular BaseTx outputs. Use the
	// signed export ID to form the source UTXO consumed by the C import.
	utxoID := avax.UTXOID{TxID: signedExport.ID(), OutputIndex: uint32(len(exportTx.Outs))}
	utxo.UTXOID = utxoID
	if err := utxo.Verify(); err != nil {
		return accountingCase{}, fmt.Errorf("UTXO verification: %w", err)
	}
	importInput := &avax.TransferableInput{
		UTXOID: utxoID,
		Asset:  avax.Asset{ID: accountingAssetID},
		In: &secp256k1fx.TransferInput{
			Amt: inputAmount,
			Input: secp256k1fx.Input{
				SigIndices: []uint32{0},
			},
		},
	}

	unsigned := &cctxtx.Import{
		NetworkID:      constants.MainnetID,
		BlockchainID:   accountingCChainID,
		SourceChain:    accountingXChainID,
		ImportedInputs: []*avax.TransferableInput{importInput},
		Outs: []cctxtx.Output{{
			Address: common.Address{1},
			Amount:  outputAmount,
			AssetID: accountingAssetID,
		}},
	}
	unsignedBytes, err := cctxtx.UnsignedBytes(unsigned)
	if err != nil {
		return accountingCase{}, fmt.Errorf("serialize unsigned import: %w", err)
	}
	sigBytes, err := sk.Sign(unsignedBytes)
	if err != nil {
		return accountingCase{}, fmt.Errorf("sign import: %w", err)
	}
	if len(sigBytes) != secp256k1.SignatureLen {
		return accountingCase{}, fmt.Errorf("signature length %d, want %d", len(sigBytes), secp256k1.SignatureLen)
	}
	var sig [secp256k1.SignatureLen]byte
	copy(sig[:], sigBytes)
	tx := &cctxtx.Tx{
		Unsigned: unsigned,
		Creds: []cctxtx.Credential{&secp256k1fx.Credential{
			Sigs: [][secp256k1.SignatureLen]byte{sig},
		}},
	}
	ctx := &snow.Context{
		NetworkID:   constants.MainnetID,
		ChainID:     accountingCChainID,
		XChainID:    accountingXChainID,
		AVAXAssetID: accountingAssetID,
	}
	if err := tx.SanityCheck(ctx); err != nil {
		return accountingCase{}, fmt.Errorf("C-Chain import sanity check: %w", err)
	}

	utxoBytes, err := cctxtx.MarshalUTXO(utxo)
	if err != nil {
		return accountingCase{}, fmt.Errorf("marshal UTXO: %w", err)
	}
	addressable, ok := utxo.Out.(avax.Addressable)
	if !ok {
		return accountingCase{}, fmt.Errorf("UTXO output is not addressable")
	}
	inputID := utxoID.InputID()
	memory := chainsatomic.NewMemory(memdb.New())
	source := memory.NewSharedMemory(accountingXChainID)
	if err := source.Apply(map[ids.ID]*chainsatomic.Requests{
		accountingCChainID: {
			PutRequests: []*chainsatomic.Element{{
				Key:    inputID[:],
				Value:  utxoBytes,
				Traits: addressable.Addresses(),
			}},
		},
	}); err != nil {
		return accountingCase{}, fmt.Errorf("seed shared memory: %w", err)
	}

	destination := memory.NewSharedMemory(accountingCChainID)
	op, err := tx.AsOp(accountingAssetID)
	if err != nil {
		return accountingCase{}, fmt.Errorf("calculate operation: %w", err)
	}
	if op.GasFeeCap.String() == "0" {
		return accountingCase{}, fmt.Errorf("import has zero gas fee cap")
	}
	importBytes, err := tx.Bytes()
	if err != nil {
		return accountingCase{}, fmt.Errorf("serialize signed import: %w", err)
	}

	return accountingCase{
		tx:                  tx,
		destination:         destination,
		utxoBytes:           utxoBytes,
		exportBytes:         exportBytes,
		unsignedImportBytes: len(unsignedBytes),
		importBytes:         importBytes,
		gas:                 uint64(op.Gas),
		traits:              len(addressable.Addresses()),
	}, nil
}

func measureVerification(c accountingCase, rounds int) (time.Duration, uint64, error) {
	for i := 0; i < 5; i++ {
		if err := c.tx.VerifyCredentials(c.destination); err != nil {
			return 0, 0, err
		}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	for i := 0; i < rounds; i++ {
		if err := c.tx.VerifyCredentials(c.destination); err != nil {
			return 0, 0, err
		}
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	return elapsed / time.Duration(rounds), (after.TotalAlloc - before.TotalAlloc) / uint64(rounds), nil
}

func measureRemoval(ownerCount, rounds int) (time.Duration, uint64, error) {
	samples := make([]removalSample, rounds)
	for i := range samples {
		c, err := newAccountingCase(ownerCount)
		if err != nil {
			return 0, 0, err
		}
		chainID, requests, err := c.tx.AtomicRequests()
		if err != nil {
			return 0, 0, err
		}
		samples[i] = removalSample{
			destination: c.destination,
			chainID:     chainID,
			requests:    requests,
		}
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	for _, sample := range samples {
		if err := sample.destination.Apply(map[ids.ID]*chainsatomic.Requests{
			sample.chainID: sample.requests,
		}); err != nil {
			return 0, 0, err
		}
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	return elapsed / time.Duration(rounds), (after.TotalAlloc - before.TotalAlloc) / uint64(rounds), nil
}
