// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package tx_test

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/stretchr/testify/require"

	chainsatomic "github.com/ava-labs/avalanchego/chains/atomic"
	"github.com/ava-labs/avalanchego/database/memdb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/vms/avm/fxs"
	avmtxs "github.com/ava-labs/avalanchego/vms/avm/txs"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	cctxtx "github.com/ava-labs/avalanchego/vms/saevm/cchain/tx"
	"github.com/ava-labs/avalanchego/vms/saevm/cchain/tx/txtest"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
)

var (
	accountingAssetID  = ids.FromStringOrPanic("FvwEAhmxKfeiG8SnEvq42hc6whRyY3EFYAvebMqDNDGCgxN5Z")
	accountingCChainID = ids.FromStringOrPanic("2q9e4r6Mu3U68nU1fYjgbR6JvwrRx36CohpAX5UQxse55x1Q5")
	accountingXChainID = ids.FromStringOrPanic("2oYMBNV4eNHyqk2fjjV5nVQLDbtmNJzq5s3qs3Lo6ftnC6FByM")
)

type accountingCase struct {
	tx          *cctxtx.Tx
	destination chainsatomic.SharedMemory
	utxoBytes   []byte
	exportBytes []byte
	importBytes []byte
	gas         uint64
	traits      int
}

// accountingOwners returns a sorted owner list whose first address is the
// signing key. The additional addresses are valid filler owners: threshold one
// means the transaction still needs only the first signature, while every
// address is serialized and checked by OutputOwners.Verify.
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

func newAccountingCase(tb testing.TB, ownerCount int) accountingCase {
	tb.Helper()
	const (
		inputAmount  = uint64(2_000_000)
		outputAmount = uint64(1_000_000)
	)

	sk, err := secp256k1.ToPrivateKey(common.FromHex("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"))
	require.NoError(tb, err)

	owners := accountingOwners(sk.Address(), ownerCount)
	utxoID := avax.UTXOID{TxID: ids.GenerateTestID()}
	utxo := &avax.UTXO{
		UTXOID: utxoID,
		Asset:  avax.Asset{ID: accountingAssetID},
		Out: &secp256k1fx.TransferOutput{
			Amt: inputAmount,
			OutputOwners: secp256k1fx.OutputOwners{
				Threshold: 1,
				Addrs:     owners,
			},
		},
	}
	require.NoError(tb, utxo.Verify())

	input := &avax.TransferableInput{
		UTXOID: utxoID,
		Asset:  avax.Asset{ID: accountingAssetID},
		In: &secp256k1fx.TransferInput{
			Amt: inputAmount,
			Input: secp256k1fx.Input{
				// accountingOwners puts the signing address at index zero.
				SigIndices: []uint32{0},
			},
		},
	}
	exportTx := &avmtxs.ExportTx{
		BaseTx: avmtxs.BaseTx{BaseTx: avax.BaseTx{
			NetworkID:    constants.MainnetID,
			BlockchainID: accountingXChainID,
			Ins:          []*avax.TransferableInput{input},
		}},
		DestinationChain: accountingCChainID,
		ExportedOuts: []*avax.TransferableOutput{{
			Asset: avax.Asset{ID: accountingAssetID},
			FxID:  secp256k1fx.ID,
			Out:   utxo.Out.(avax.TransferableOut),
		}},
	}
	parser, err := avmtxs.NewParser([]fxs.Fx{&secp256k1fx.Fx{}})
	require.NoError(tb, err)
	unsignedExportBytes, err := parser.Codec().Marshal(avmtxs.CodecVersion, exportTx)
	require.NoError(tb, err)
	signedExport := &avmtxs.Tx{Unsigned: exportTx}
	require.NoError(tb, signedExport.SignSECP256K1Fx(parser.Codec(), [][]*secp256k1.PrivateKey{{sk}}))
	exportBytes := signedExport.Bytes()
	require.Greater(tb, len(exportBytes), len(unsignedExportBytes))
	require.Less(tb, len(exportBytes), 64*1024)
	unsigned := &cctxtx.Import{
		NetworkID:      constants.MainnetID,
		BlockchainID:   accountingCChainID,
		SourceChain:    accountingXChainID,
		ImportedInputs: []*avax.TransferableInput{input},
		Outs: []cctxtx.Output{{
			Address: common.Address{1},
			Amount:  outputAmount,
			AssetID: accountingAssetID,
		}},
	}
	tx := &cctxtx.Tx{
		Unsigned: unsigned,
		Creds: []cctxtx.Credential{&secp256k1fx.Credential{
			Sigs: []txtest.Signature{txtest.Sign(tb, unsigned, sk)},
		}},
	}
	require.NoError(tb, tx.SanityCheck(&snow.Context{
		NetworkID:   constants.MainnetID,
		ChainID:     accountingCChainID,
		XChainID:    accountingXChainID,
		AVAXAssetID: accountingAssetID,
	}))

	utxoBytes := txtest.MarshalUTXO(tb, utxo)
	inputID := utxoID.InputID()
	addressable, ok := utxo.Out.(avax.Addressable)
	require.True(tb, ok)
	traits := addressable.Addresses()
	element := &chainsatomic.Element{
		Key:    inputID[:],
		Value:  utxoBytes,
		Traits: traits,
	}

	memory := chainsatomic.NewMemory(memdb.New())
	source := memory.NewSharedMemory(accountingXChainID)
	require.NoError(tb, source.Apply(map[ids.ID]*chainsatomic.Requests{
		accountingCChainID: {PutRequests: []*chainsatomic.Element{element}},
	}))

	destination := memory.NewSharedMemory(accountingCChainID)
	op, err := tx.AsOp(accountingAssetID)
	require.NoError(tb, err)
	// Keep the case fee-paying: the input exceeds the destination output by
	// 1,000,000 nAVAX, matching the ordinary atomic fee denomination.
	require.NotEqual(tb, "0", op.GasFeeCap.String())
	importBytes, err := tx.Bytes()
	require.NoError(tb, err)

	return accountingCase{
		tx:          tx,
		destination: destination,
		utxoBytes:   utxoBytes,
		exportBytes: exportBytes,
		importBytes: importBytes,
		gas:         uint64(op.Gas),
		traits:      len(traits),
	}
}

func TestAtomicImportExternalPayloadAccounting(t *testing.T) {
	ownerCounts := []int{1, 32, 256, 1024, 2048}
	var baselineGas uint64
	for i, ownerCount := range ownerCounts {
		t.Run(fmt.Sprintf("owners_%d", ownerCount), func(t *testing.T) {
			c := newAccountingCase(t, ownerCount)
			require.NoError(t, c.tx.VerifyCredentials(c.destination))

			t.Logf("owners=%d traits=%d source_utxo_bytes=%d source_export_bytes=%d signed_import_bytes=%d charged_gas=%d", c.traits, c.traits, len(c.utxoBytes), len(c.exportBytes), len(c.importBytes), c.gas)
			if i == 0 {
				baselineGas = c.gas
				return
			}
			// The source UTXO is external to the C transaction. Its owner list
			// changes the bytes fetched and checked, but not the C descriptor or
			// signature count used by AsOp().
			require.Equal(t, baselineGas, c.gas)
			require.Greater(t, len(c.utxoBytes), 0)
		})
	}
}

// BenchmarkAtomicImportExternalPayload measures the repeated destination-side
// verification that the C txpool and block hook perform. Run with -benchmem to
// record allocations as well as wall-clock cost.
func BenchmarkAtomicImportExternalPayload(b *testing.B) {
	for _, ownerCount := range []int{1, 32, 256, 1024, 2048} {
		b.Run(fmt.Sprintf("owners_%d", ownerCount), func(b *testing.B) {
			c := newAccountingCase(b, ownerCount)
			b.ReportMetric(float64(len(c.utxoBytes)), "external_bytes")
			b.ReportMetric(float64(len(c.exportBytes)), "source_export_bytes")
			b.ReportMetric(float64(len(c.importBytes)), "import_bytes")
			b.ReportMetric(float64(c.traits), "traits")
			b.ReportMetric(float64(c.gas), "charged_gas")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := c.tx.VerifyCredentials(c.destination); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAtomicImportTraitRemoval measures the shared-memory operation that
// consumes the imported UTXO. The setup (including the PutRequests that create
// the owner indexes) is stopped out of the timer; the measured Apply call only
// removes the value and deletes one index entry per owner/trait.
func BenchmarkAtomicImportTraitRemoval(b *testing.B) {
	for _, ownerCount := range []int{1, 32, 256, 1024, 2048} {
		b.Run(fmt.Sprintf("owners_%d", ownerCount), func(b *testing.B) {
			sample := newAccountingCase(b, ownerCount)
			b.ReportMetric(float64(len(sample.utxoBytes)), "external_bytes")
			b.ReportMetric(float64(sample.traits), "traits")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				c := newAccountingCase(b, ownerCount)
				chainID, requests, err := c.tx.AtomicRequests()
				require.NoError(b, err)
				b.StartTimer()
				if err := c.destination.Apply(map[ids.ID]*chainsatomic.Requests{chainID: requests}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
