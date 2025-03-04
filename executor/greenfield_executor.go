package executor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	_ "encoding/json"
	"fmt"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	crosschaintypes "github.com/cosmos/cosmos-sdk/x/crosschain/types"
	oracletypes "github.com/cosmos/cosmos-sdk/x/oracle/types"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/prysmaticlabs/prysm/crypto/bls/blst"
	"github.com/spf13/viper"
	"github.com/tendermint/tendermint/rpc/client"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	tmtypes "github.com/tendermint/tendermint/types"
	"github.com/tendermint/tendermint/votepool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	sdkclient "github.com/bnb-chain/greenfield-go-sdk/client/chain"
	sdkkeys "github.com/bnb-chain/greenfield-go-sdk/keys"
	sdktypes "github.com/bnb-chain/greenfield-go-sdk/types"
	relayercommon "github.com/bnb-chain/greenfield-relayer/common"
	"github.com/bnb-chain/greenfield-relayer/config"
	"github.com/bnb-chain/greenfield-relayer/logging"
	"github.com/bnb-chain/greenfield-relayer/types"
)

type GreenfieldExecutor struct {
	BscExecutor   *BSCExecutor
	gnfdClients   *sdkclient.GnfdCompositeClients
	config        *config.Config
	address       string
	validators    []*tmtypes.Validator // used to cache validators
	cdc           *codec.ProtoCodec
	BlsPrivateKey []byte
	BlsPubKey     []byte
}

func NewGreenfieldExecutor(cfg *config.Config) *GreenfieldExecutor {
	privKey := viper.GetString(config.FlagConfigPrivateKey)
	if privKey == "" {
		privKey = getGreenfieldPrivateKey(&cfg.GreenfieldConfig)
	}
	km, err := sdkkeys.NewPrivateKeyManager(privKey)
	if err != nil {
		panic(err)
	}

	blsPrivKeyStr := viper.GetString(config.FlagConfigBlsPrivateKey)
	if blsPrivKeyStr == "" {
		blsPrivKeyStr = getGreenfieldBlsPrivateKey(&cfg.GreenfieldConfig)
	}
	blsPrivKeyBts := ethcommon.Hex2Bytes(blsPrivKeyStr)

	blsPrivKey, err := blst.SecretKeyFromBytes(blsPrivKeyBts)
	if err != nil {
		panic(err)
	}
	clients := sdkclient.NewGnfdCompositClients(
		cfg.GreenfieldConfig.GRPCAddrs,
		cfg.GreenfieldConfig.RPCAddrs,
		cfg.GreenfieldConfig.ChainIdString,
		sdkclient.WithKeyManager(km),
		sdkclient.WithGrpcDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	return &GreenfieldExecutor{
		gnfdClients:   clients,
		address:       km.GetAddr().String(),
		config:        cfg,
		cdc:           Cdc(),
		BlsPrivateKey: blsPrivKeyBts,
		BlsPubKey:     blsPrivKey.PublicKey().Marshal(),
	}
}

func (e *GreenfieldExecutor) SetBSCExecutor(be *BSCExecutor) {
	e.BscExecutor = be
}

func getGreenfieldPrivateKey(cfg *config.GreenfieldConfig) string {
	if cfg.KeyType == config.KeyTypeAWSPrivateKey {
		result, err := config.GetSecret(cfg.AWSSecretName, cfg.AWSRegion)
		if err != nil {
			panic(err)
		}
		type AwsPrivateKey struct {
			PrivateKey string `json:"private_key"`
		}
		var awsPrivateKey AwsPrivateKey
		err = json.Unmarshal([]byte(result), &awsPrivateKey)
		if err != nil {
			panic(err)
		}
		return awsPrivateKey.PrivateKey
	}
	return cfg.PrivateKey
}

func getGreenfieldBlsPrivateKey(cfg *config.GreenfieldConfig) string {
	if cfg.KeyType == config.KeyTypeAWSPrivateKey {
		result, err := config.GetSecret(cfg.AWSBlsSecretName, cfg.AWSRegion)
		if err != nil {
			panic(err)
		}
		type AwsPrivateKey struct {
			PrivateKey string `json:"bls_private_key"`
		}
		var awsBlsPrivateKey AwsPrivateKey
		err = json.Unmarshal([]byte(result), &awsBlsPrivateKey)
		if err != nil {
			panic(err)
		}
		return awsBlsPrivateKey.PrivateKey
	}
	return cfg.BlsPrivateKey
}

func (e *GreenfieldExecutor) getRpcClient() client.Client {
	return e.gnfdClients.GetClient().TendermintClient.RpcClient.TmClient
}

func (e *GreenfieldExecutor) GetGnfdClient() *sdkclient.GreenfieldClient {
	return e.gnfdClients.GetClient().GreenfieldClient
}

func (e *GreenfieldExecutor) GetBlockAndBlockResultAtHeight(height int64) (*tmtypes.Block, *ctypes.ResultBlockResults, error) {
	block, err := e.getRpcClient().Block(context.Background(), &height)
	if err != nil {
		return nil, nil, err
	}
	blockResults, err := e.getRpcClient().BlockResults(context.Background(), &height)
	if err != nil {
		return nil, nil, err
	}
	return block.Block, blockResults, nil
}

func (e *GreenfieldExecutor) GetLatestBlockHeight() (latestHeight uint64, err error) {
	return uint64(e.gnfdClients.GetClient().Height), nil
}

func (e *GreenfieldExecutor) QueryTendermintLightBlock(height int64) ([]byte, error) {
	validators, err := e.getRpcClient().Validators(context.Background(), &height, nil, nil)
	if err != nil {
		return nil, err
	}
	commit, err := e.getRpcClient().Commit(context.Background(), &height)
	if err != nil {
		return nil, err
	}
	validatorSet := tmtypes.NewValidatorSet(validators.Validators)
	if err != nil {
		return nil, err
	}
	lightBlock := tmtypes.LightBlock{
		SignedHeader: &commit.SignedHeader,
		ValidatorSet: validatorSet,
	}
	protoBlock, err := lightBlock.ToProto()
	if err != nil {
		return nil, err
	}
	return protoBlock.Marshal()
}

// GetNextDeliverySequenceForChannelWithRetry calls dest chain(BSC) to return a sequence # which should be used.
func (e *GreenfieldExecutor) GetNextDeliverySequenceForChannelWithRetry(channelID types.ChannelId) (sequence uint64, err error) {
	return sequence, retry.Do(func() error {
		sequence, err = e.getNextDeliverySequenceForChannel(channelID)
		return err
	}, relayercommon.RtyAttem,
		relayercommon.RtyDelay,
		relayercommon.RtyErr,
		retry.OnRetry(func(n uint, err error) {
			logging.Logger.Errorf("failed to query receive sequence for channel %d, attempt: %d times, max_attempts: %d", channelID, n+1, relayercommon.RtyAttNum)
		}))
}

func (e *GreenfieldExecutor) getNextDeliverySequenceForChannel(channelID types.ChannelId) (uint64, error) {
	sequence, err := e.BscExecutor.GetNextReceiveSequenceForChannelWithRetry(channelID)
	if err != nil {
		return 0, err
	}
	return sequence, nil
}

// GetNextSendSequenceForChannelWithRetry gets the next send sequence of specified channel from Greenfield
func (e *GreenfieldExecutor) GetNextSendSequenceForChannelWithRetry(channelID types.ChannelId) (sequence uint64, err error) {
	return sequence, retry.Do(func() error {
		sequence, err = e.getNextSendSequenceForChannel(channelID)
		return err
	}, relayercommon.RtyAttem,
		relayercommon.RtyDelay,
		relayercommon.RtyErr,
		retry.OnRetry(func(n uint, err error) {
			logging.Logger.Errorf("failed to query send sequence for channel %d, attempt: %d times, max_attempts: %d", channelID, n+1, relayercommon.RtyAttNum)
		}))
}

func (e *GreenfieldExecutor) getNextSendSequenceForChannel(channelId types.ChannelId) (uint64, error) {
	res, err := e.GetGnfdClient().SendSequence(
		context.Background(),
		&crosschaintypes.QuerySendSequenceRequest{ChannelId: uint32(channelId)},
	)
	if err != nil {
		return 0, err
	}
	return res.Sequence, nil
}

// GetNextReceiveOracleSequence gets the next receive Oracle sequence from Greenfield
func (e *GreenfieldExecutor) GetNextReceiveOracleSequence() (uint64, error) {
	res, err := e.GetGnfdClient().CrosschainQueryClient.ReceiveSequence(
		context.Background(),
		&crosschaintypes.QueryReceiveSequenceRequest{ChannelId: uint32(relayercommon.OracleChannelId)},
	)
	if err != nil {
		return 0, err
	}
	return res.Sequence, nil
}

// GetNextReceiveSequenceForChannel gets the sequence specifically for bsc -> gnfd package's channel from Greenfield
func (e *GreenfieldExecutor) GetNextReceiveSequenceForChannel(channelId types.ChannelId) (uint64, error) {
	res, err := e.GetGnfdClient().ReceiveSequence(
		context.Background(),
		&crosschaintypes.QueryReceiveSequenceRequest{ChannelId: uint32(channelId)},
	)
	if err != nil {
		return 0, err
	}
	return res.Sequence, nil
}

func (e *GreenfieldExecutor) queryLatestValidators() ([]*tmtypes.Validator, error) {
	validators, err := e.getRpcClient().Validators(context.Background(), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return validators.Validators, nil
}

func (e *GreenfieldExecutor) QueryValidatorsAtHeight(height uint64) ([]*tmtypes.Validator, error) {
	h := int64(height)
	validators, err := e.getRpcClient().Validators(context.Background(), &h, nil, nil)
	if err != nil {
		return nil, err
	}
	return validators.Validators, nil
}

func (e *GreenfieldExecutor) QueryCachedLatestValidators() ([]*tmtypes.Validator, error) {
	if len(e.validators) != 0 {
		return e.validators, nil
	}
	validators, err := e.queryLatestValidators()
	if err != nil {
		return nil, err
	}
	return validators, nil
}

func (e *GreenfieldExecutor) UpdateCachedLatestValidatorsLoop() {
	ticker := time.NewTicker(UpdateCachedValidatorsInterval)
	for range ticker.C {
		validators, err := e.queryLatestValidators()
		if err != nil {
			logging.Logger.Errorf("update latest greenfield validators error, err=%s", err)
			continue
		}
		e.validators = validators
	}
}

func (e *GreenfieldExecutor) GetValidatorsBlsPublicKey() ([]string, error) {
	validators, err := e.QueryCachedLatestValidators()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, v := range validators {
		keys = append(keys, hex.EncodeToString(v.BlsKey))
	}
	return keys, nil
}

func (e *GreenfieldExecutor) GetNonce() (uint64, error) {
	return e.GetGnfdClient().GetNonce()
}

func (e *GreenfieldExecutor) ClaimPackages(client *sdkclient.GreenfieldClient, payloadBts []byte, aggregatedSig []byte, voteAddressSet []uint64, claimTs int64, oracleSeq uint64, nonce uint64) (string, error) {
	msgClaim := oracletypes.NewMsgClaim(
		e.address,
		e.getSrcChainId(),
		e.getDestChainId(),
		oracleSeq,
		uint64(claimTs),
		payloadBts,
		voteAddressSet,
		aggregatedSig,
	)
	txRes, err := client.BroadcastTx(
		[]sdk.Msg{msgClaim},
		&sdktypes.TxOption{
			NoSimulate: true,
			GasLimit:   e.config.GreenfieldConfig.GasLimit,
			FeeAmount:  sdk.NewCoins(sdk.NewCoin(sdktypes.Denom, sdk.NewInt(int64(e.config.GreenfieldConfig.FeeAmount)))),
			Nonce:      nonce,
		},
	)
	if err != nil {
		return "", err
	}
	if txRes.TxResponse.Code != 0 {
		return "", fmt.Errorf("claim error, code=%d, log=%s", txRes.TxResponse.Code, txRes.TxResponse.RawLog)
	}
	return txRes.TxResponse.TxHash, nil
}

func (e *GreenfieldExecutor) GetInturnRelayer() (*oracletypes.QueryInturnRelayerResponse, error) {
	return e.GetGnfdClient().OracleQueryClient.InturnRelayer(context.Background(), &oracletypes.QueryInturnRelayerRequest{})
}

func (e *GreenfieldExecutor) QueryVotesByEventHashAndType(eventHash []byte, eventType votepool.EventType) ([]*votepool.Vote, error) {
	queryMap := make(map[string]interface{})
	queryMap[VotePoolQueryParameterEventType] = int(eventType)
	queryMap[VotePoolQueryParameterEventHash] = eventHash
	var queryVote ctypes.ResultQueryVote
	_, err := e.gnfdClients.GetClient().JsonRpcClient.Call(context.Background(), VotePoolQueryMethodName, queryMap, &queryVote)
	if err != nil {
		return nil, err
	}
	return queryVote.Votes, nil
}

func (e *GreenfieldExecutor) BroadcastVote(v *votepool.Vote) error {
	broadcastMap := make(map[string]interface{})
	broadcastMap[VotePoolBroadcastParameterKey] = *v
	_, err := e.gnfdClients.GetClient().JsonRpcClient.Call(context.Background(), VotePoolBroadcastMethodName, broadcastMap, &ctypes.ResultBroadcastVote{})
	if err != nil {
		return err
	}
	return nil
}

func (e *GreenfieldExecutor) getDestChainId() uint32 {
	return uint32(e.config.GreenfieldConfig.ChainId)
}

func (e *GreenfieldExecutor) getSrcChainId() uint32 {
	return uint32(e.config.BSCConfig.ChainId)
}
