package offchainreporting2

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/smartcontractkit/chainlink/core/services/ocrcommon"
	"github.com/smartcontractkit/chainlink/core/services/postgres"

	"gorm.io/gorm"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	gethCommon "github.com/ethereum/go-ethereum/common"
	gethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
	offchain_aggregator_wrapper "github.com/smartcontractkit/chainlink/core/internal/gethwrappers2/generated/offchainaggregator"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	httypes "github.com/smartcontractkit/chainlink/core/services/headtracker/types"
	"github.com/smartcontractkit/chainlink/core/services/log"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/smartcontractkit/chainlink/core/utils"
	"github.com/smartcontractkit/libocr/gethwrappers2/ocr2aggregator"
	"github.com/smartcontractkit/libocr/offchainreporting2/chains/evmutil"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2/types"
)

// configMailboxSanityLimit is the maximum number of configs that can be held
// in the mailbox. Under normal operation there should never be more than 0 or
// 1 configs in the mailbox, this limit is here merely to prevent unbounded usage
// in some kind of unforeseen insane situation.
const configMailboxSanityLimit = 100

var (
	_ ocrtypes.ContractConfigTracker = &OCRContractTracker{}
	_ httypes.HeadTrackable          = &OCRContractTracker{}

	OCRContractConfigSet            = getEventTopic("ConfigSet")
	OCRContractLatestRoundRequested = getEventTopic("RoundRequested")
)

//go:generate mockery --name OCRContractTrackerDB --output ./mocks/ --case=underscore
type (
	// OCRContractTracker complies with ContractConfigTracker interface and
	// handles log events related to the contract more generally
	OCRContractTracker struct {
		utils.StartStopOnce

		ethClient eth.Client
		// XXX: Use the libocr provided Filterer and Caller so that the resulting
		// ConfigSet can be passed to ContractConfigFromConfigSetEvent, the
		// offchain_aggregator_wrapper for Topics and contract Address
		contract         *offchain_aggregator_wrapper.OffchainAggregator
		contractFilterer *ocr2aggregator.OCR2AggregatorFilterer
		contractCaller   *ocr2aggregator.OCR2AggregatorCaller
		logBroadcaster   log.Broadcaster
		jobID            int32
		logger           logger.Logger
		db               OCRContractTrackerDB
		gdb              *gorm.DB
		blockTranslator  ocrcommon.BlockTranslator
		chain            ocrcommon.Chain

		// HeadBroadcaster
		headBroadcaster  httypes.HeadBroadcaster
		unsubscribeHeads func()

		// Start/Stop lifecycle
		ctx             context.Context
		ctxCancel       context.CancelFunc
		wg              sync.WaitGroup
		unsubscribeLogs func()

		// LatestRoundRequested
		latestRoundRequested ocr2aggregator.OCR2AggregatorRoundRequested
		lrrMu                sync.RWMutex

		// ContractConfig
		configsMB utils.Mailbox
		chConfigs chan ocrtypes.ContractConfig

		// LatestBlockHeight
		latestBlockHeight   int64
		latestBlockHeightMu sync.RWMutex
	}

	OCRContractTrackerDB interface {
		SaveLatestRoundRequested(tx *sql.Tx, rr ocr2aggregator.OCR2AggregatorRoundRequested) error
		LoadLatestRoundRequested() (rr ocr2aggregator.OCR2AggregatorRoundRequested, err error)
	}
)

// NewOCRContractTracker makes a new OCRContractTracker
func NewOCRContractTracker(
	contract *offchain_aggregator_wrapper.OffchainAggregator,
	contractFilterer *ocr2aggregator.OCR2AggregatorFilterer,
	contractCaller *ocr2aggregator.OCR2AggregatorCaller,
	ethClient eth.Client,
	logBroadcaster log.Broadcaster,
	jobID int32,
	logger logger.Logger,
	gdb *gorm.DB,
	db OCRContractTrackerDB,
	chain ocrcommon.Chain,
	headBroadcaster httypes.HeadBroadcaster,
) (o *OCRContractTracker) {
	ctx, cancel := context.WithCancel(context.Background())
	return &OCRContractTracker{
		utils.StartStopOnce{},
		ethClient,
		contract,
		contractFilterer,
		contractCaller,
		logBroadcaster,
		jobID,
		logger,
		db,
		gdb,
		ocrcommon.NewBlockTranslator(chain, ethClient),
		chain,
		headBroadcaster,
		nil,
		ctx,
		cancel,
		sync.WaitGroup{},
		nil,
		ocr2aggregator.OCR2AggregatorRoundRequested{},
		sync.RWMutex{},
		*utils.NewMailbox(configMailboxSanityLimit),
		make(chan ocrtypes.ContractConfig),
		-1,
		sync.RWMutex{},
	}
}

// Start must be called before logs can be delivered
// It ought to be called before starting OCR
func (t *OCRContractTracker) Start() error {
	return t.StartOnce("OCRContractTracker", func() (err error) {
		t.latestRoundRequested, err = t.db.LoadLatestRoundRequested()
		if err != nil {
			return errors.Wrap(err, "OCRContractTracker#Start: failed to load latest round requested")
		}

		t.unsubscribeLogs = t.logBroadcaster.Register(t, log.ListenerOpts{
			Contract: t.contract.Address(),
			ParseLog: t.contract.ParseLog,
			LogsWithTopics: map[gethCommon.Hash][][]log.Topic{
				offchain_aggregator_wrapper.OffchainAggregatorRoundRequested{}.Topic(): nil,
				offchain_aggregator_wrapper.OffchainAggregatorConfigSet{}.Topic():      nil,
			},
			NumConfirmations: 1,
		})

		var latestHead *models.Head
		latestHead, t.unsubscribeHeads = t.headBroadcaster.Subscribe(t)
		if latestHead != nil {
			t.setLatestBlockHeight(*latestHead)
		}

		t.wg.Add(1)
		go t.processLogs()
		return nil
	})
}

// Close should be called after teardown of the OCR job relying on this tracker
func (t *OCRContractTracker) Close() error {
	return t.StopOnce("OCRContractTracker", func() error {
		t.ctxCancel()
		t.wg.Wait()
		t.unsubscribeHeads()
		t.unsubscribeLogs()
		close(t.chConfigs)
		return nil
	})
}

// Connect conforms to HeadTrackable
func (t *OCRContractTracker) Connect(*models.Head) error { return nil }

// OnNewLongestChain conformed to HeadTrackable and updates latestBlockHeight
func (t *OCRContractTracker) OnNewLongestChain(_ context.Context, h models.Head) {
	t.setLatestBlockHeight(h)
}

func (t *OCRContractTracker) setLatestBlockHeight(h models.Head) {
	var num int64
	if h.L1BlockNumber.Valid {
		num = h.L1BlockNumber.Int64
	} else {
		num = h.Number
	}
	t.latestBlockHeightMu.Lock()
	defer t.latestBlockHeightMu.Unlock()
	if num > t.latestBlockHeight {
		t.latestBlockHeight = num
	}
}

func (t *OCRContractTracker) getLatestBlockHeight() int64 {
	t.latestBlockHeightMu.RLock()
	defer t.latestBlockHeightMu.RUnlock()
	return t.latestBlockHeight
}

func (t *OCRContractTracker) processLogs() {
	defer t.wg.Done()
	for {
		select {
		case <-t.configsMB.Notify():
			// NOTE: libocr could take an arbitrary amount of time to process a
			// new config. To avoid blocking the log broadcaster, we use this
			// background thread to deliver them and a mailbox as the buffer.
			for {
				x, exists := t.configsMB.Retrieve()
				if !exists {
					break
				}
				cc, ok := x.(ocrtypes.ContractConfig)
				if !ok {
					panic(fmt.Sprintf("expected ocrtypes.ContractConfig but got %T", x))
				}
				select {
				case t.chConfigs <- cc:
				case <-t.ctx.Done():
					return
				}
			}
		case <-t.ctx.Done():
			return
		}
	}
}

// HandleLog complies with LogListener interface
// It is not thread safe
func (t *OCRContractTracker) HandleLog(lb log.Broadcast) {
	was, err := t.logBroadcaster.WasAlreadyConsumed(t.gdb, lb)
	if err != nil {
		t.logger.Errorw("OCRContract: could not determine if log was already consumed", "error", err)
		return
	} else if was {
		return
	}

	raw := lb.RawLog()
	if raw.Address != t.contract.Address() {
		t.logger.Errorf("log address of 0x%x does not match configured contract address of 0x%x", raw.Address, t.contract.Address())
		t.logger.ErrorIfCalling(func() error { return t.logBroadcaster.MarkConsumed(t.gdb, lb) })
		return
	}
	topics := raw.Topics
	if len(topics) == 0 {
		t.logger.ErrorIfCalling(func() error { return t.logBroadcaster.MarkConsumed(t.gdb, lb) })
		return
	}

	var consumed bool
	switch topics[0] {
	case offchain_aggregator_wrapper.OffchainAggregatorConfigSet{}.Topic():
		var configSet *ocr2aggregator.OCR2AggregatorConfigSet
		configSet, err = t.contractFilterer.ParseConfigSet(raw)
		if err != nil {
			t.logger.Errorw("could not parse config set", "err", err)
			t.logger.ErrorIfCalling(func() error { return t.logBroadcaster.MarkConsumed(t.gdb, lb) })
			return
		}
		configSet.Raw = lb.RawLog()
		cc := evmutil.ContractConfigFromConfigSetEvent(*configSet)

		wasOverCapacity := t.configsMB.Deliver(cc)
		if wasOverCapacity {
			t.logger.Error("config mailbox is over capacity - dropped the oldest unprocessed item")
		}
	case offchain_aggregator_wrapper.OffchainAggregatorRoundRequested{}.Topic():
		var rr *ocr2aggregator.OCR2AggregatorRoundRequested
		rr, err = t.contractFilterer.ParseRoundRequested(raw)
		if err != nil {
			t.logger.Errorw("could not parse round requested", "err", err)
			t.logger.ErrorIfCalling(func() error { return t.logBroadcaster.MarkConsumed(t.gdb, lb) })
			return
		}
		if IsLaterThan(raw, t.latestRoundRequested.Raw) {
			err = postgres.GormTransactionWithDefaultContext(t.gdb, func(tx *gorm.DB) error {
				if err = t.db.SaveLatestRoundRequested(postgres.MustSQLTx(tx), *rr); err != nil {
					return err
				}
				return t.logBroadcaster.MarkConsumed(tx, lb)
			})
			if err != nil {
				logger.Error(err)
				return
			}
			consumed = true
			t.lrrMu.Lock()
			t.latestRoundRequested = *rr
			t.lrrMu.Unlock()
			t.logger.Infow("OCRContractTracker: received new latest RoundRequested event", "latestRoundRequested", *rr)
		} else {
			t.logger.Warnw("OCRContractTracker: ignoring out of date RoundRequested event", "latestRoundRequested", t.latestRoundRequested, "roundRequested", rr)
		}
	default:
		logger.Debugw("OCRContractTracker: got unrecognised log topic", "topic", topics[0])
	}
	if !consumed {
		ctx, cancel := postgres.DefaultQueryCtx()
		defer cancel()
		t.logger.ErrorIfCalling(func() error { return t.logBroadcaster.MarkConsumed(t.gdb.WithContext(ctx), lb) })
	}
}

// IsLaterThan returns true if the first log was emitted "after" the second log
// from the blockchain's point of view
func IsLaterThan(incoming gethTypes.Log, existing gethTypes.Log) bool {
	return incoming.BlockNumber > existing.BlockNumber ||
		(incoming.BlockNumber == existing.BlockNumber && incoming.TxIndex > existing.TxIndex) ||
		(incoming.BlockNumber == existing.BlockNumber && incoming.TxIndex == existing.TxIndex && incoming.Index > existing.Index)
}

// IsV2Job complies with LogListener interface
func (t *OCRContractTracker) IsV2Job() bool {
	return true
}

// JobID complies with LogListener interface
func (t *OCRContractTracker) JobID() int32 {
	return t.jobID
}

// Notify returns a channel that can wake up the contract tracker to let it
// know when a new config is available
func (t *OCRContractTracker) Notify() <-chan struct{} {
	return nil
}

// LatestConfigDetails queries the eth node
func (t *OCRContractTracker) LatestConfigDetails(ctx context.Context) (changedInBlock uint64, configDigest ocrtypes.ConfigDigest, err error) {
	var cancel context.CancelFunc
	ctx, cancel = utils.CombinedContext(t.ctx, ctx)
	defer cancel()

	opts := bind.CallOpts{Context: ctx, Pending: false}
	result, err := t.contract.LatestConfigDetails(&opts)
	if err != nil {
		return 0, configDigest, errors.Wrap(err, "error getting LatestConfigDetails")
	}
	configDigest, err = ocrtypes.BytesToConfigDigest(result.ConfigDigest[:])
	if err != nil {
		return 0, configDigest, errors.Wrap(err, "error getting config digest")
	}
	return uint64(result.BlockNumber), configDigest, err
}

// Return the latest configuration
func (t *OCRContractTracker) LatestConfig(ctx context.Context, changedInBlock uint64) (ocrtypes.ContractConfig, error) {
	fromBlock, toBlock := t.blockTranslator.NumberToQueryRange(ctx, changedInBlock)
	q := ethereum.FilterQuery{
		FromBlock: fromBlock,
		ToBlock:   toBlock,
		Addresses: []gethCommon.Address{t.contract.Address()},
		Topics: [][]gethCommon.Hash{
			{OCRContractConfigSet},
		},
	}

	var cancel context.CancelFunc
	ctx, cancel = utils.CombinedContext(t.ctx, ctx)
	defer cancel()

	logs, err := t.ethClient.FilterLogs(ctx, q)
	if err != nil {
		return ocrtypes.ContractConfig{}, err
	}
	if len(logs) == 0 {
		return ocrtypes.ContractConfig{}, errors.Errorf("ConfigFromLogs: OCRContract with address 0x%x has no logs", t.contract.Address())
	}

	latest, err := t.contractFilterer.ParseConfigSet(logs[len(logs)-1])
	if err != nil {
		return ocrtypes.ContractConfig{}, errors.Wrap(err, "ConfigFromLogs failed to ParseConfigSet")
	}
	latest.Raw = logs[len(logs)-1]
	if latest.Raw.Address != t.contract.Address() {
		return ocrtypes.ContractConfig{}, errors.Errorf("log address of 0x%x does not match configured contract address of 0x%x", latest.Raw.Address, t.contract.Address())
	}
	return evmutil.ContractConfigFromConfigSetEvent(*latest), err
}

// LatestBlockHeight queries the eth node for the most recent header
func (t *OCRContractTracker) LatestBlockHeight(ctx context.Context) (blockheight uint64, err error) {
	// We skip confirmation checking anyway on Optimism so there's no need to
	// care about the block height; we have no way of getting the L1 block
	// height anyway
	if t.chain.IsOptimism() {
		return 0, nil
	}
	latestBlockHeight := t.getLatestBlockHeight()
	if latestBlockHeight >= 0 {
		return uint64(latestBlockHeight), nil
	}

	t.logger.Debugw("OCRContractTracker: still waiting for first head, falling back to on-chain lookup")

	var cancel context.CancelFunc
	ctx, cancel = utils.CombinedContext(t.ctx, ctx)
	defer cancel()

	h, err := t.ethClient.HeadByNumber(ctx, nil)
	if err != nil {
		return 0, err
	}
	if h == nil {
		return 0, errors.New("got nil head")
	}

	if h.L1BlockNumber.Valid {
		return uint64(h.L1BlockNumber.Int64), nil
	}

	return uint64(h.Number), nil
}

// LatestRoundRequested returns the configDigest, epoch, and round from the latest
// RoundRequested event emitted by the contract. LatestRoundRequested may or may not
// return a result if the latest such event was emitted in a block b such that
// b.timestamp < tip.timestamp - lookback.
//
// If no event is found, LatestRoundRequested should return zero values, not an error.
// An error should only be returned if an actual error occurred during execution,
// e.g. because there was an error querying the blockchain or the database.
//
// As an optimization, this function may also return zero values, if no
// RoundRequested event has been emitted after the latest NewTransmission event.
func (t *OCRContractTracker) LatestRoundRequested(_ context.Context, lookback time.Duration) (configDigest ocrtypes.ConfigDigest, epoch uint32, round uint8, err error) {
	// NOTE: This should be "good enough" 99% of the time.
	// It guarantees validity up to `BLOCK_BACKFILL_DEPTH` blocks ago
	// Some further improvements could be made:
	// TODO: Can we increase the backfill depth?
	// TODO: Can we use the lookback to optimise at all?
	// TODO: How well can we satisfy the requirements after the latest round of changes to the log broadcaster?
	// See: https://www.pivotaltracker.com/story/show/177063733
	t.lrrMu.RLock()
	defer t.lrrMu.RUnlock()
	return t.latestRoundRequested.ConfigDigest, t.latestRoundRequested.Epoch, t.latestRoundRequested.Round, nil
}

func getEventTopic(name string) gethCommon.Hash {
	abi, err := abi.JSON(strings.NewReader(ocr2aggregator.OCR2AggregatorABI))
	if err != nil {
		panic("could not parse OffchainAggregator ABI: " + err.Error())
	}
	event, exists := abi.Events[name]
	if !exists {
		panic(fmt.Sprintf("abi.Events was missing %s", name))
	}
	return event.ID
}