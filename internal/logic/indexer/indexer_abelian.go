package indexer

import (
	"errors"
	"fmt"

	"github.com/b2network/b2-indexer/internal/types"
	"github.com/b2network/b2-indexer/pkg/log"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// AbelianIndexer bitcoin indexer, parse and forward data
type AbelianIndexer struct {
	client              *rpcclient.Client // call bitcoin rpc client
	chainParams         *chaincfg.Params  // bitcoin network params, e.g. mainnet, testnet, etc.
	listenAddress       btcutil.Address   // need listened bitcoin address
	targetConfirmations uint64
	logger              log.Logger
}

// NewAbelianIndexer new bitcoin indexer
func NewAbelianIndexer(
	log log.Logger,
	client *rpcclient.Client,
	chainParams *chaincfg.Params,
	listenAddress string,
	targetConfirmations uint64,
) (types.BITCOINTxIndexer, error) {
	// check listenAddress
	address, err := btcutil.DecodeAddress(listenAddress, chainParams)
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrDecodeListenAddress, err.Error())
	}
	return &AbelianIndexer{
		logger:              log,
		client:              client,
		chainParams:         chainParams,
		listenAddress:       address,
		targetConfirmations: targetConfirmations,
	}, nil
}

// ParseBlock parse block data by block height
// NOTE: Currently, only transfer transactions are supported.
func (b *AbelianIndexer) ParseBlock(height int64, txIndex int64) ([]*types.BitcoinTxParseResult, *wire.BlockHeader, error) {
	blockResult, err := b.GetBlockByHeight(height)
	if err != nil {
		return nil, nil, err
	}

	blockParsedResult := make([]*types.BitcoinTxParseResult, 0)
	for k, v := range blockResult.Transactions {
		if int64(k) < txIndex {
			continue
		}

		b.logger.Debugw("parse block", "k", k, "height", height, "txIndex", txIndex, "tx", v.TxHash().String())

		parseTxs, err := b.parseTx(v, k)
		if err != nil {
			return nil, nil, err
		}
		b.logger.Infof("parse block:height=%v,txIndex=%v", height, k)

		if parseTxs != nil {
			blockParsedResult = append(blockParsedResult, parseTxs)
		}
	}

	return blockParsedResult, &blockResult.Header, nil
}

func (b *AbelianIndexer) CheckConfirmations(hash string) error {
	txVerbose, err := b.GetRawTransactionVerbose(hash)
	if err != nil {
		return err
	}

	if txVerbose.Confirmations < b.targetConfirmations {
		return fmt.Errorf("%w, current confirmations:%d target confirmations: %d",
			ErrTargetConfirmations, txVerbose.Confirmations, b.targetConfirmations)
	}
	return nil
}

// parseTx parse transaction data
func (b *AbelianIndexer) parseTx(txResult *wire.MsgTx, index int) (*types.BitcoinTxParseResult, error) {
	//listenAddress := false
	//var totalValue int64
	//tos := make([]types.BitcoinTo, 0)
	tos, totalValue, listenAddress, _ := b.parseToAddress(txResult.TxOut)
	if listenAddress {
		fromAddress, err := b.parseFromAddress(txResult.TxIn)
		if err != nil {
			return nil, fmt.Errorf("vin parse err:%w", err)
		}

		// TODO: temp fix, if from is listened address, continue
		if len(fromAddress) == 0 {
			b.logger.Warnw("parse from address empty or nonsupport tx type",
				"txId", txResult.TxHash().String(),
				"listenAddress", b.listenAddress.EncodeAddress())
			return nil, nil
		}

		return &types.BitcoinTxParseResult{
			TxID:   txResult.TxHash().String(),
			TxType: TxTypeTransfer,
			Index:  int64(index),
			Value:  totalValue,
			From:   fromAddress,
			To:     b.listenAddress.EncodeAddress(),
			Tos:    tos,
		}, nil
	}
	return nil, nil
}
func (b *AbelianIndexer) parseToAddress(TxOut []*wire.TxOut) (toAddress []types.BitcoinTo, value int64, listenAddress bool, err error) {
	hasListenAddress := false
	var totalValue int64
	tos := make([]types.BitcoinTo, 0)

	for _, v := range TxOut {
		pkAddress, err := b.parseAddress(v.PkScript)
		if err != nil {
			if errors.Is(err, ErrParsePkScript) {
				continue
			}
			// null data
			if errors.Is(err, ErrParsePkScriptNullData) {
				continue
			}
			return nil, 0, false, err
		}
		parseTo := types.BitcoinTo{
			Address: pkAddress,
			Value:   v.Value,
		}
		tos = append(tos, parseTo)
		// if pk address eq dest listened address, after parse from address by vin prev tx
		if pkAddress == b.listenAddress.EncodeAddress() {
			hasListenAddress = true
			totalValue += v.Value
		}
	}

	return tos, totalValue, hasListenAddress, nil
}

// parseFromAddress from vin parse from address
// return all possible values parsed from address
// TODO: at present, it is assumed that it is a single from, and multiple from needs to be tested later
func (b *AbelianIndexer) parseFromAddress(TxIn []*wire.TxIn) (fromAddress []types.BitcoinFrom, err error) {
	for _, vin := range TxIn {
		// get prev tx hash
		prevTxID := vin.PreviousOutPoint.Hash
		if prevTxID.String() == "0000000000000000000000000000000000000000000000000000000000000000" {
			return nil, nil
		}
		vinResult, err := b.GetRawTransaction(&prevTxID)
		if err != nil {
			return nil, fmt.Errorf("vin get raw transaction err:%w", err)
		}
		if len(vinResult.MsgTx().TxOut) == 0 {
			return nil, fmt.Errorf("vin txOut is null")
		}
		vinPKScript := vinResult.MsgTx().TxOut[vin.PreviousOutPoint.Index].PkScript
		//  script to address
		vinPkAddress, err := b.parseAddress(vinPKScript)
		if err != nil {
			b.logger.Errorw("vin parse address", "error", err)
			if errors.Is(err, ErrParsePkScript) || errors.Is(err, ErrParsePkScriptNullData) {
				continue
			}
			return nil, err
		}

		fromAddress = append(fromAddress, types.BitcoinFrom{
			Address: vinPkAddress,
		})
	}
	return fromAddress, nil
}

// parseAddress from pkscript parse address
func (b *AbelianIndexer) ParseAddress(pkScript []byte) (string, error) {
	return b.parseAddress(pkScript)
}

// parseNullData from pkscript parse null data
//
//lint:ignore U1000 Ignore unused function temporarily for debugging
func (b *AbelianIndexer) parseNullData(pkScript []byte) (string, error) {
	pk, err := txscript.ParsePkScript(pkScript)
	if err != nil {
		return "", fmt.Errorf("%w:%s", ErrParsePkScript, err.Error())
	}
	if pk.Class() != txscript.NullDataTy {
		return "", fmt.Errorf("not null data type")
	}
	return pk.String(), nil
}

// parseAddress from pkscript parse address
func (b *AbelianIndexer) parseAddress(pkScript []byte) (string, error) {
	pk, err := txscript.ParsePkScript(pkScript)
	if err != nil {
		return "", fmt.Errorf("%w:%s", ErrParsePkScript, err.Error())
	}

	if pk.Class() == txscript.NullDataTy {
		return "", ErrParsePkScriptNullData
	}

	//  encodes the script into an address for the given chain.
	pkAddress, err := pk.Address(b.chainParams)
	if err != nil {
		return "", fmt.Errorf("PKScript to address err:%w", err)
	}
	return pkAddress.EncodeAddress(), nil
}

// LatestBlock get latest block height in the longest block chain.
func (b *AbelianIndexer) LatestBlock() (int64, error) {
	return b.client.GetBlockCount()
}

// BlockChainInfo get block chain info
func (b *AbelianIndexer) BlockChainInfo() (*btcjson.GetBlockChainInfoResult, error) {
	return b.client.GetBlockChainInfo()
}

func (b *AbelianIndexer) GetRawTransactionVerbose(hash string) (*btcjson.TxRawResult, error) {
	txHash, err := chainhash.NewHashFromStr(hash)
	if err != nil {
		return nil, err
	}
	txVerbose, err := b.client.GetRawTransactionVerbose(txHash)
	if err != nil {
		return nil, err
	}
	return txVerbose, nil
}

func (b *AbelianIndexer) GetRawTransaction(txHash *chainhash.Hash) (*btcutil.Tx, error) {
	return b.client.GetRawTransaction(txHash)
}

// GetBlockByHeight returns a raw block from the server given its height
func (b *AbelianIndexer) GetBlockByHeight(height int64) (*wire.MsgBlock, error) {
	blockhash, err := b.client.GetBlockHash(height)
	if err != nil {
		return nil, err
	}
	msgBlock, err := b.client.GetBlock(blockhash)
	if err != nil {
		return nil, err
	}
	return msgBlock, nil
}
