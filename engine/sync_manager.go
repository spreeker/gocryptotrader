package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/thrasher-corp/gocryptotrader/common"
	"github.com/thrasher-corp/gocryptotrader/config"
	"github.com/thrasher-corp/gocryptotrader/currency"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/orderbook"
	"github.com/thrasher-corp/gocryptotrader/exchanges/stats"
	"github.com/thrasher-corp/gocryptotrader/exchanges/ticker"
	"github.com/thrasher-corp/gocryptotrader/log"
)

// const holds the sync item types
const (
	SyncItemTicker = iota
	SyncItemOrderbook
	SyncItemTrade
	SyncManagerName = "exchange_syncer"
)

var (
	createdCounter = 0
	removedCounter = 0
	// DefaultSyncerWorkers limits the number of sync workers
	DefaultSyncerWorkers = 15
	// DefaultSyncerTimeoutREST the default time to switch from REST to websocket protocols without a response
	DefaultSyncerTimeoutREST = time.Second * 15
	// DefaultSyncerTimeoutWebsocket the default time to switch from websocket to REST protocols without a response
	DefaultSyncerTimeoutWebsocket = time.Minute
	errNoSyncItemsEnabled         = errors.New("no sync items enabled")
	errUnknownSyncItem            = errors.New("unknown sync item")
	errSyncPairNotFound           = errors.New("exchange currency pair syncer not found")
	errCouldNotSyncNewData        = errors.New("could not sync new data")
)

// setupSyncManager starts a new CurrencyPairSyncer
func setupSyncManager(c *SyncManagerConfig, exchangeManager iExchangeManager, remoteConfig *config.RemoteControlConfig, websocketRoutineManagerEnabled bool) (*syncManager, error) {
	if c == nil {
		return nil, fmt.Errorf("%T %w", c, common.ErrNilPointer)
	}

	if !c.SynchronizeOrderbook && !c.SynchronizeTicker && !c.SynchronizeTrades {
		return nil, errNoSyncItemsEnabled
	}
	if exchangeManager == nil {
		return nil, errNilExchangeManager
	}
	if remoteConfig == nil {
		return nil, errNilConfig
	}

	if c.NumWorkers <= 0 {
		c.NumWorkers = DefaultSyncerWorkers
	}

	if c.TimeoutREST <= time.Duration(0) {
		c.TimeoutREST = DefaultSyncerTimeoutREST
	}

	if c.TimeoutWebsocket <= time.Duration(0) {
		c.TimeoutWebsocket = DefaultSyncerTimeoutWebsocket
	}

	if c.FiatDisplayCurrency.IsEmpty() {
		return nil, fmt.Errorf("FiatDisplayCurrency %w", currency.ErrCurrencyCodeEmpty)
	}

	if !c.FiatDisplayCurrency.IsFiatCurrency() {
		return nil, fmt.Errorf("%s %w", c.FiatDisplayCurrency, currency.ErrFiatDisplayCurrencyIsNotFiat)
	}

	if c.PairFormatDisplay == nil {
		return nil, fmt.Errorf("%T %w", c.PairFormatDisplay, common.ErrNilPointer)
	}

	s := &syncManager{
		config:                         *c,
		remoteConfig:                   remoteConfig,
		exchangeManager:                exchangeManager,
		websocketRoutineManagerEnabled: websocketRoutineManagerEnabled,
		fiatDisplayCurrency:            c.FiatDisplayCurrency,
		format:                         *c.PairFormatDisplay,
		tickerBatchLastRequested:       make(map[string]time.Time),
	}

	log.Debugf(log.SyncMgr,
		"Exchange currency pair syncer config: continuous: %v ticker: %v"+
			" orderbook: %v trades: %v workers: %v verbose: %v timeout REST: %v"+
			" timeout Websocket: %v",
		s.config.SynchronizeContinuously, s.config.SynchronizeTicker, s.config.SynchronizeOrderbook,
		s.config.SynchronizeTrades, s.config.NumWorkers, s.config.Verbose, s.config.TimeoutREST,
		s.config.TimeoutWebsocket)
	s.inService.Add(1)
	return s, nil
}

// IsRunning safely checks whether the subsystem is running
func (m *syncManager) IsRunning() bool {
	return m != nil && atomic.LoadInt32(&m.started) == 1
}

// Start runs the subsystem
func (m *syncManager) Start() error {
	if m == nil {
		return fmt.Errorf("exchange CurrencyPairSyncer %w", ErrNilSubsystem)
	}
	if !atomic.CompareAndSwapInt32(&m.started, 0, 1) {
		return ErrSubSystemAlreadyStarted
	}
	m.initSyncWG.Add(1)
	m.inService.Done()
	log.Debugln(log.SyncMgr, "Exchange CurrencyPairSyncer started.")
	exchanges, err := m.exchangeManager.GetExchanges()
	if err != nil {
		return err
	}
	for x := range exchanges {
		exchangeName := exchanges[x].GetName()
		supportsWebsocket := exchanges[x].SupportsWebsocket()
		supportsREST := exchanges[x].SupportsREST()

		if !supportsREST && !supportsWebsocket {
			log.Warnf(log.SyncMgr,
				"Loaded exchange %s does not support REST or Websocket.",
				exchangeName)
			continue
		}

		var usingWebsocket bool
		var usingREST bool
		if m.websocketRoutineManagerEnabled &&
			supportsWebsocket &&
			exchanges[x].IsWebsocketEnabled() {
			usingWebsocket = true
		} else if supportsREST {
			usingREST = true
		}

		assetTypes := exchanges[x].GetAssetTypes(false)
		for y := range assetTypes {
			if exchanges[x].GetBase().CurrencyPairs.IsAssetEnabled(assetTypes[y]) != nil {
				log.Warnf(log.SyncMgr,
					"%s asset type %s is disabled, fetching enabled pairs is paused",
					exchangeName,
					assetTypes[y])
				continue
			}

			wsAssetSupported := exchanges[x].IsAssetWebsocketSupported(assetTypes[y])
			if !wsAssetSupported {
				log.Warnf(log.SyncMgr,
					"%s asset type %s websocket functionality is unsupported, REST fetching only.",
					exchangeName,
					assetTypes[y])
			}
			enabledPairs, err := exchanges[x].GetEnabledPairs(assetTypes[y])
			if err != nil {
				log.Errorf(log.SyncMgr,
					"%s failed to get enabled pairs. Err: %s",
					exchangeName,
					err)
				continue
			}
			for i := range enabledPairs {
				if m.exists(exchangeName, enabledPairs[i], assetTypes[y]) {
					continue
				}

				c := &currencyPairSyncAgent{
					AssetType: assetTypes[y],
					Exchange:  exchangeName,
					Pair:      enabledPairs[i],
				}
				sBase := syncBase{
					IsUsingREST:      usingREST || !wsAssetSupported,
					IsUsingWebsocket: usingWebsocket && wsAssetSupported,
				}
				if m.config.SynchronizeTicker {
					c.Ticker = sBase
				}
				if m.config.SynchronizeOrderbook {
					c.Orderbook = sBase
				}
				if m.config.SynchronizeTrades {
					c.Trade = sBase
				}

				m.add(c)
			}
		}
	}

	if atomic.CompareAndSwapInt32(&m.initSyncStarted, 0, 1) {
		log.Debugf(log.SyncMgr,
			"Exchange CurrencyPairSyncer initial sync started. %d items to process.",
			createdCounter)
		m.initSyncStartTime = time.Now()
	}

	go func() {
		m.initSyncWG.Wait()
		if atomic.CompareAndSwapInt32(&m.initSyncCompleted, 0, 1) {
			log.Debugf(log.SyncMgr, "Exchange CurrencyPairSyncer initial sync is complete.")
			completedTime := time.Now()
			log.Debugf(log.SyncMgr, "Exchange CurrencyPairSyncer initial sync took %v [%v sync items].",
				completedTime.Sub(m.initSyncStartTime), createdCounter)

			if !m.config.SynchronizeContinuously {
				log.Debugln(log.SyncMgr, "Exchange CurrencyPairSyncer stopping.")
				err := m.Stop()
				if err != nil {
					log.Errorln(log.SyncMgr, err)
				}
				return
			}
		}
	}()

	if atomic.LoadInt32(&m.initSyncCompleted) == 1 && !m.config.SynchronizeContinuously {
		return nil
	}

	for i := 0; i < m.config.NumWorkers; i++ {
		go m.worker()
	}
	m.initSyncWG.Done()
	return nil
}

// Stop shuts down the exchange currency pair syncer
func (m *syncManager) Stop() error {
	if m == nil {
		return fmt.Errorf("exchange CurrencyPairSyncer %w", ErrNilSubsystem)
	}
	if !atomic.CompareAndSwapInt32(&m.started, 1, 0) {
		return fmt.Errorf("exchange CurrencyPairSyncer %w", ErrSubSystemNotStarted)
	}
	m.inService.Add(1)
	log.Debugln(log.SyncMgr, "Exchange CurrencyPairSyncer stopped.")
	return nil
}

func (m *syncManager) get(exchangeName string, p currency.Pair, a asset.Item) (*currencyPairSyncAgent, error) {
	m.mux.Lock()
	defer m.mux.Unlock()

	for x := range m.currencyPairs {
		if m.currencyPairs[x].Exchange == exchangeName &&
			m.currencyPairs[x].Pair.Equal(p) &&
			m.currencyPairs[x].AssetType == a {
			return &m.currencyPairs[x], nil
		}
	}

	return nil, fmt.Errorf("%v %v %v %w", exchangeName, a, p, errSyncPairNotFound)
}

func (m *syncManager) exists(exchangeName string, p currency.Pair, a asset.Item) bool {
	m.mux.Lock()
	defer m.mux.Unlock()

	for x := range m.currencyPairs {
		if m.currencyPairs[x].Exchange == exchangeName &&
			m.currencyPairs[x].Pair.Equal(p) &&
			m.currencyPairs[x].AssetType == a {
			return true
		}
	}
	return false
}

func (m *syncManager) add(c *currencyPairSyncAgent) {
	m.mux.Lock()
	defer m.mux.Unlock()

	if m.config.SynchronizeTicker {
		if m.config.Verbose {
			log.Debugf(log.SyncMgr,
				"%s: Added ticker sync item %v: using websocket: %v using REST: %v",
				c.Exchange, m.FormatCurrency(c.Pair).String(), c.Ticker.IsUsingWebsocket,
				c.Ticker.IsUsingREST)
		}
		if atomic.LoadInt32(&m.initSyncCompleted) != 1 {
			m.initSyncWG.Add(1)
			createdCounter++
		}
	}

	if m.config.SynchronizeOrderbook {
		if m.config.Verbose {
			log.Debugf(log.SyncMgr,
				"%s: Added orderbook sync item %v: using websocket: %v using REST: %v",
				c.Exchange, m.FormatCurrency(c.Pair).String(), c.Orderbook.IsUsingWebsocket,
				c.Orderbook.IsUsingREST)
		}
		if atomic.LoadInt32(&m.initSyncCompleted) != 1 {
			m.initSyncWG.Add(1)
			createdCounter++
		}
	}

	if m.config.SynchronizeTrades {
		if m.config.Verbose {
			log.Debugf(log.SyncMgr,
				"%s: Added trade sync item %v: using websocket: %v using REST: %v",
				c.Exchange, m.FormatCurrency(c.Pair).String(), c.Trade.IsUsingWebsocket,
				c.Trade.IsUsingREST)
		}
		if atomic.LoadInt32(&m.initSyncCompleted) != 1 {
			m.initSyncWG.Add(1)
			createdCounter++
		}
	}

	c.Created = time.Now()
	m.currencyPairs = append(m.currencyPairs, *c)
}

func (m *syncManager) isProcessing(exchangeName string, p currency.Pair, a asset.Item, syncType int) bool {
	m.mux.Lock()
	defer m.mux.Unlock()

	for x := range m.currencyPairs {
		if m.currencyPairs[x].Exchange == exchangeName &&
			m.currencyPairs[x].Pair.Equal(p) &&
			m.currencyPairs[x].AssetType == a {
			switch syncType {
			case SyncItemTicker:
				return m.currencyPairs[x].Ticker.IsProcessing
			case SyncItemOrderbook:
				return m.currencyPairs[x].Orderbook.IsProcessing
			case SyncItemTrade:
				return m.currencyPairs[x].Trade.IsProcessing
			}
		}
	}

	return false
}

func (m *syncManager) setProcessing(exchangeName string, p currency.Pair, a asset.Item, syncType int, processing bool) {
	m.mux.Lock()
	defer m.mux.Unlock()

	for x := range m.currencyPairs {
		if m.currencyPairs[x].Exchange == exchangeName &&
			m.currencyPairs[x].Pair.Equal(p) &&
			m.currencyPairs[x].AssetType == a {
			switch syncType {
			case SyncItemTicker:
				m.currencyPairs[x].Ticker.IsProcessing = processing
			case SyncItemOrderbook:
				m.currencyPairs[x].Orderbook.IsProcessing = processing
			case SyncItemTrade:
				m.currencyPairs[x].Trade.IsProcessing = processing
			}
		}
	}
}

// Update notifies the syncManager to change the last updated time for a exchange asset pair
func (m *syncManager) Update(exchangeName string, p currency.Pair, a asset.Item, syncType int, err error) error {
	if m == nil {
		return fmt.Errorf("exchange CurrencyPairSyncer %w", ErrNilSubsystem)
	}
	if atomic.LoadInt32(&m.started) == 0 {
		return fmt.Errorf("exchange CurrencyPairSyncer %w", ErrSubSystemNotStarted)
	}

	if atomic.LoadInt32(&m.initSyncStarted) != 1 {
		return nil
	}

	switch syncType {
	case SyncItemOrderbook:
		if !m.config.SynchronizeOrderbook {
			return nil
		}
	case SyncItemTicker:
		if !m.config.SynchronizeTicker {
			return nil
		}
	case SyncItemTrade:
		if !m.config.SynchronizeTrades {
			return nil
		}
	default:
		return fmt.Errorf("%v %w", syncType, errUnknownSyncItem)
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	for x := range m.currencyPairs {
		if m.currencyPairs[x].Exchange == exchangeName &&
			m.currencyPairs[x].Pair.Equal(p) &&
			m.currencyPairs[x].AssetType == a {
			switch syncType {
			case SyncItemTicker:
				origHadData := m.currencyPairs[x].Ticker.HaveData
				m.currencyPairs[x].Ticker.LastUpdated = time.Now()
				if err != nil {
					m.currencyPairs[x].Ticker.NumErrors++
				}
				m.currencyPairs[x].Ticker.HaveData = true
				m.currencyPairs[x].Ticker.IsProcessing = false
				if atomic.LoadInt32(&m.initSyncCompleted) != 1 && !origHadData {
					removedCounter++
					log.Debugf(log.SyncMgr, "%s ticker sync complete %v [%d/%d].",
						exchangeName,
						m.FormatCurrency(p).String(),
						removedCounter,
						createdCounter)
					m.initSyncWG.Done()
				}
				return nil
			case SyncItemOrderbook:
				origHadData := m.currencyPairs[x].Orderbook.HaveData
				m.currencyPairs[x].Orderbook.LastUpdated = time.Now()
				if err != nil {
					m.currencyPairs[x].Orderbook.NumErrors++
				}
				m.currencyPairs[x].Orderbook.HaveData = true
				m.currencyPairs[x].Orderbook.IsProcessing = false
				if atomic.LoadInt32(&m.initSyncCompleted) != 1 && !origHadData {
					removedCounter++
					log.Debugf(log.SyncMgr, "%s orderbook sync complete %v [%d/%d].",
						exchangeName,
						m.FormatCurrency(p).String(),
						removedCounter,
						createdCounter)
					m.initSyncWG.Done()
				}
				return nil
			case SyncItemTrade:
				origHadData := m.currencyPairs[x].Trade.HaveData
				m.currencyPairs[x].Trade.LastUpdated = time.Now()
				if err != nil {
					m.currencyPairs[x].Trade.NumErrors++
				}
				m.currencyPairs[x].Trade.HaveData = true
				m.currencyPairs[x].Trade.IsProcessing = false
				if atomic.LoadInt32(&m.initSyncCompleted) != 1 && !origHadData {
					removedCounter++
					log.Debugf(log.SyncMgr, "%s trade sync complete %v [%d/%d].",
						exchangeName,
						m.FormatCurrency(p).String(),
						removedCounter,
						createdCounter)
					m.initSyncWG.Done()
				}
				return nil
			}
		}
	}
	return fmt.Errorf("%w for %s %s %s", errCouldNotSyncNewData, exchangeName, p, a)
}

func (m *syncManager) worker() {
	cleanup := func() {
		log.Debugln(log.SyncMgr,
			"Exchange CurrencyPairSyncer worker shutting down.")
	}
	defer cleanup()

	for atomic.LoadInt32(&m.started) != 0 {
		exchanges, err := m.exchangeManager.GetExchanges()
		if err != nil {
			log.Errorf(log.SyncMgr, "Sync manager cannot get exchanges: %v", err)
		}
		for x := range exchanges {
			exchangeName := exchanges[x].GetName()
			supportsREST := exchanges[x].SupportsREST()
			supportsRESTTickerBatching := exchanges[x].SupportsRESTTickerBatchUpdates()
			var usingREST bool
			var usingWebsocket bool
			var switchedToRest bool
			if exchanges[x].SupportsWebsocket() && exchanges[x].IsWebsocketEnabled() {
				ws, err := exchanges[x].GetWebsocket()
				if err != nil {
					log.Errorf(log.SyncMgr,
						"%s unable to get websocket pointer. Err: %s",
						exchangeName,
						err)
					usingREST = true
				}

				if ws.IsConnected() {
					usingWebsocket = true
				} else {
					usingREST = true
				}
			} else if supportsREST {
				usingREST = true
			}

			assetTypes := exchanges[x].GetAssetTypes(true)
			for y := range assetTypes {
				wsAssetSupported := exchanges[x].IsAssetWebsocketSupported(assetTypes[y])
				enabledPairs, err := exchanges[x].GetEnabledPairs(assetTypes[y])
				if err != nil {
					log.Errorf(log.SyncMgr,
						"%s failed to get enabled pairs. Err: %s",
						exchangeName,
						err)
					continue
				}
				for i := range enabledPairs {
					if atomic.LoadInt32(&m.started) == 0 {
						return
					}

					c, err := m.get(exchangeName, enabledPairs[i], assetTypes[y])
					if err != nil {
						if err == errSyncPairNotFound {
							c = &currencyPairSyncAgent{
								AssetType: assetTypes[y],
								Exchange:  exchangeName,
								Pair:      enabledPairs[i],
							}

							sBase := syncBase{
								IsUsingREST:      usingREST || !wsAssetSupported,
								IsUsingWebsocket: usingWebsocket && wsAssetSupported,
							}

							if m.config.SynchronizeTicker {
								c.Ticker = sBase
							}

							if m.config.SynchronizeOrderbook {
								c.Orderbook = sBase
							}

							if m.config.SynchronizeTrades {
								c.Trade = sBase
							}

							m.add(c)
						} else {
							log.Errorln(log.SyncMgr, err)
							continue
						}
					}
					if switchedToRest && usingWebsocket {
						log.Warnf(log.SyncMgr,
							"%s %s: Websocket re-enabled, switching from rest to websocket",
							c.Exchange, m.FormatCurrency(enabledPairs[i]).String())
						switchedToRest = false
					}

					if m.config.SynchronizeOrderbook {
						if !m.isProcessing(exchangeName, c.Pair, c.AssetType, SyncItemOrderbook) {
							if c.Orderbook.LastUpdated.IsZero() ||
								(time.Since(c.Orderbook.LastUpdated) > m.config.TimeoutREST && c.Orderbook.IsUsingREST) ||
								(time.Since(c.Orderbook.LastUpdated) > m.config.TimeoutWebsocket && c.Orderbook.IsUsingWebsocket) {
								if c.Orderbook.IsUsingWebsocket {
									if time.Since(c.Created) < m.config.TimeoutWebsocket {
										continue
									}
									if supportsREST {
										m.setProcessing(c.Exchange, c.Pair, c.AssetType, SyncItemOrderbook, true)
										c.Orderbook.IsUsingWebsocket = false
										c.Orderbook.IsUsingREST = true
										log.Warnf(log.SyncMgr,
											"%s %s %s: No orderbook update after %s, switching from websocket to rest",
											c.Exchange,
											m.FormatCurrency(c.Pair).String(),
											strings.ToUpper(c.AssetType.String()),
											m.config.TimeoutWebsocket,
										)
										switchedToRest = true
										m.setProcessing(c.Exchange, c.Pair, c.AssetType, SyncItemOrderbook, false)
									}
								}

								m.setProcessing(c.Exchange, c.Pair, c.AssetType, SyncItemOrderbook, true)
								result, err := exchanges[x].UpdateOrderbook(context.TODO(),
									c.Pair,
									c.AssetType)
								m.PrintOrderbookSummary(result, "REST", err)
								if err == nil {
									if m.remoteConfig.WebsocketRPC.Enabled {
										relayWebsocketEvent(result, "orderbook_update", c.AssetType.String(), exchangeName)
									}
								}
								updateErr := m.Update(c.Exchange, c.Pair, c.AssetType, SyncItemOrderbook, err)
								if updateErr != nil {
									log.Errorln(log.SyncMgr, updateErr)
								}
							} else {
								time.Sleep(time.Millisecond * 50)
							}
						}
					}
					if m.config.SynchronizeTicker {
						if !m.isProcessing(exchangeName, c.Pair, c.AssetType, SyncItemTicker) {
							if c.Ticker.LastUpdated.IsZero() ||
								(time.Since(c.Ticker.LastUpdated) > m.config.TimeoutREST && c.Ticker.IsUsingREST) ||
								(time.Since(c.Ticker.LastUpdated) > m.config.TimeoutWebsocket && c.Ticker.IsUsingWebsocket) {
								if c.Ticker.IsUsingWebsocket {
									if time.Since(c.Created) < m.config.TimeoutWebsocket {
										continue
									}

									if supportsREST {
										m.setProcessing(c.Exchange, c.Pair, c.AssetType, SyncItemTicker, true)
										c.Ticker.IsUsingWebsocket = false
										c.Ticker.IsUsingREST = true
										log.Warnf(log.SyncMgr,
											"%s %s %s: No ticker update after %s, switching from websocket to rest",
											c.Exchange,
											m.FormatCurrency(enabledPairs[i]).String(),
											strings.ToUpper(c.AssetType.String()),
											m.config.TimeoutWebsocket,
										)
										switchedToRest = true
										m.setProcessing(c.Exchange, c.Pair, c.AssetType, SyncItemTicker, false)
									}
								}

								if c.Ticker.IsUsingREST {
									m.setProcessing(c.Exchange, c.Pair, c.AssetType, SyncItemTicker, true)
									var result *ticker.Price
									var err error

									if supportsRESTTickerBatching {
										m.mux.Lock()
										batchLastDone, ok := m.tickerBatchLastRequested[exchangeName]
										if !ok {
											m.tickerBatchLastRequested[exchangeName] = time.Time{}
										}
										m.mux.Unlock()

										if batchLastDone.IsZero() || time.Since(batchLastDone) > m.config.TimeoutREST {
											m.mux.Lock()
											if m.config.Verbose {
												log.Debugf(log.SyncMgr, "Initialising %s REST ticker batching", exchangeName)
											}
											err = exchanges[x].UpdateTickers(context.TODO(), c.AssetType)
											if err == nil {
												result, err = exchanges[x].FetchTicker(context.TODO(), c.Pair, c.AssetType)
											}
											m.tickerBatchLastRequested[exchangeName] = time.Now()
											m.mux.Unlock()
										} else {
											if m.config.Verbose {
												log.Debugf(log.SyncMgr, "%s Using recent batching cache", exchangeName)
											}
											result, err = exchanges[x].FetchTicker(context.TODO(),
												c.Pair,
												c.AssetType)
										}
									} else {
										result, err = exchanges[x].UpdateTicker(context.TODO(),
											c.Pair,
											c.AssetType)
									}
									m.PrintTickerSummary(result, "REST", err)
									if err == nil {
										if m.remoteConfig.WebsocketRPC.Enabled {
											relayWebsocketEvent(result, "ticker_update", c.AssetType.String(), exchangeName)
										}
									}
									updateErr := m.Update(c.Exchange, c.Pair, c.AssetType, SyncItemTicker, err)
									if updateErr != nil {
										log.Errorln(log.SyncMgr, updateErr)
									}
								}
							} else {
								time.Sleep(time.Millisecond * 50)
							}
						}
					}

					if m.config.SynchronizeTrades {
						if !m.isProcessing(exchangeName, c.Pair, c.AssetType, SyncItemTrade) {
							if c.Trade.LastUpdated.IsZero() || time.Since(c.Trade.LastUpdated) > m.config.TimeoutREST {
								m.setProcessing(c.Exchange, c.Pair, c.AssetType, SyncItemTrade, true)
								err := m.Update(c.Exchange, c.Pair, c.AssetType, SyncItemTrade, nil)
								if err != nil {
									log.Errorln(log.SyncMgr, err)
								}
							}
						}
					}
				}
			}
		}
	}
}

func printCurrencyFormat(price float64, displayCurrency currency.Code) string {
	displaySymbol, err := currency.GetSymbolByCurrencyName(displayCurrency)
	if err != nil {
		log.Errorf(log.SyncMgr, "Failed to get display symbol: %s", err)
	}

	return fmt.Sprintf("%s%.8f", displaySymbol, price)
}

func printConvertCurrencyFormat(origPrice float64, origCurrency, displayCurrency currency.Code) string {
	var conv float64
	if origPrice > 0 {
		var err error
		conv, err = currency.ConvertFiat(origPrice, origCurrency, displayCurrency)
		if err != nil {
			log.Errorf(log.SyncMgr, "Failed to convert currency: %s", err)
		}
	}

	displaySymbol, err := currency.GetSymbolByCurrencyName(displayCurrency)
	if err != nil {
		log.Errorf(log.SyncMgr, "Failed to get display symbol: %s", err)
	}

	origSymbol, err := currency.GetSymbolByCurrencyName(origCurrency)
	if err != nil {
		log.Errorf(log.SyncMgr, "Failed to get original currency symbol for %s: %s",
			origCurrency,
			err)
	}

	return fmt.Sprintf("%s%.2f %s (%s%.2f %s)",
		displaySymbol,
		conv,
		displayCurrency,
		origSymbol,
		origPrice,
		origCurrency,
	)
}

// PrintTickerSummary outputs the ticker results
func (m *syncManager) PrintTickerSummary(result *ticker.Price, protocol string, err error) {
	if m == nil || atomic.LoadInt32(&m.started) == 0 {
		return
	}
	if err != nil {
		if err == common.ErrNotYetImplemented {
			log.Warnf(log.SyncMgr, "Failed to get %s ticker. Error: %s",
				protocol,
				err)
			return
		}
		log.Errorf(log.SyncMgr, "Failed to get %s ticker. Error: %s",
			protocol,
			err)
		return
	}

	// ignoring error as not all tickers have volume populated and error is not actionable
	_ = stats.Add(result.ExchangeName, result.Pair, result.AssetType, result.Last, result.Volume)

	if result.Pair.Quote.IsFiatCurrency() &&
		!result.Pair.Quote.Equal(m.fiatDisplayCurrency) &&
		!m.fiatDisplayCurrency.IsEmpty() {
		origCurrency := result.Pair.Quote.Upper()
		log.Infof(log.SyncMgr, "%s %s %s %s TICKER: Last %s Ask %s Bid %s High %s Low %s Volume %.8f",
			result.ExchangeName,
			protocol,
			m.FormatCurrency(result.Pair),
			strings.ToUpper(result.AssetType.String()),
			printConvertCurrencyFormat(result.Last, origCurrency, m.fiatDisplayCurrency),
			printConvertCurrencyFormat(result.Ask, origCurrency, m.fiatDisplayCurrency),
			printConvertCurrencyFormat(result.Bid, origCurrency, m.fiatDisplayCurrency),
			printConvertCurrencyFormat(result.High, origCurrency, m.fiatDisplayCurrency),
			printConvertCurrencyFormat(result.Low, origCurrency, m.fiatDisplayCurrency),
			result.Volume)
	} else {
		if result.Pair.Quote.IsFiatCurrency() &&
			result.Pair.Quote.Equal(m.fiatDisplayCurrency) &&
			!m.fiatDisplayCurrency.IsEmpty() {
			log.Infof(log.SyncMgr, "%s %s %s %s TICKER: Last %s Ask %s Bid %s High %s Low %s Volume %.8f",
				result.ExchangeName,
				protocol,
				m.FormatCurrency(result.Pair),
				strings.ToUpper(result.AssetType.String()),
				printCurrencyFormat(result.Last, m.fiatDisplayCurrency),
				printCurrencyFormat(result.Ask, m.fiatDisplayCurrency),
				printCurrencyFormat(result.Bid, m.fiatDisplayCurrency),
				printCurrencyFormat(result.High, m.fiatDisplayCurrency),
				printCurrencyFormat(result.Low, m.fiatDisplayCurrency),
				result.Volume)
		} else {
			log.Infof(log.SyncMgr, "%s %s %s %s TICKER: Last %.8f Ask %.8f Bid %.8f High %.8f Low %.8f Volume %.8f",
				result.ExchangeName,
				protocol,
				m.FormatCurrency(result.Pair),
				strings.ToUpper(result.AssetType.String()),
				result.Last,
				result.Ask,
				result.Bid,
				result.High,
				result.Low,
				result.Volume)
		}
	}
}

// FormatCurrency is a method that formats and returns a currency pair
// based on the user currency display preferences
func (m *syncManager) FormatCurrency(p currency.Pair) currency.Pair {
	if m == nil || atomic.LoadInt32(&m.started) == 0 {
		return p
	}
	return p.Format(m.format)
}

const (
	book = "%s %s %s %s ORDERBOOK: Bids len: %d Amount: %f %s. Total value: %s Asks len: %d Amount: %f %s. Total value: %s"
)

// PrintOrderbookSummary outputs orderbook results
func (m *syncManager) PrintOrderbookSummary(result *orderbook.Base, protocol string, err error) {
	if m == nil || atomic.LoadInt32(&m.started) == 0 {
		return
	}
	if err != nil {
		if result == nil {
			log.Errorf(log.OrderBook, "Failed to get %s orderbook. Error: %s",
				protocol,
				err)
			return
		}
		if err == common.ErrNotYetImplemented {
			log.Warnf(log.OrderBook, "Failed to get %s orderbook for %s %s %s. Error: %s",
				protocol,
				result.Exchange,
				result.Pair,
				result.Asset,
				err)
			return
		}
		log.Errorf(log.OrderBook, "Failed to get %s orderbook for %s %s %s. Error: %s",
			protocol,
			result.Exchange,
			result.Pair,
			result.Asset,
			err)
		return
	}

	bidsAmount, bidsValue := result.TotalBidsAmount()
	asksAmount, asksValue := result.TotalAsksAmount()

	var bidValueResult, askValueResult string
	switch {
	case result.Pair.Quote.IsFiatCurrency() && !result.Pair.Quote.Equal(m.fiatDisplayCurrency) && !m.fiatDisplayCurrency.IsEmpty():
		origCurrency := result.Pair.Quote.Upper()
		if bidsValue > 0 {
			bidValueResult = printConvertCurrencyFormat(bidsValue, origCurrency, m.fiatDisplayCurrency)
		}
		if asksValue > 0 {
			askValueResult = printConvertCurrencyFormat(asksValue, origCurrency, m.fiatDisplayCurrency)
		}
	case result.Pair.Quote.IsFiatCurrency() && result.Pair.Quote.Equal(m.fiatDisplayCurrency) && !m.fiatDisplayCurrency.IsEmpty():
		bidValueResult = printCurrencyFormat(bidsValue, m.fiatDisplayCurrency)
		askValueResult = printCurrencyFormat(asksValue, m.fiatDisplayCurrency)
	default:
		bidValueResult = strconv.FormatFloat(bidsValue, 'f', -1, 64)
		askValueResult = strconv.FormatFloat(asksValue, 'f', -1, 64)
	}

	log.Infof(log.SyncMgr, book,
		result.Exchange,
		protocol,
		m.FormatCurrency(result.Pair),
		strings.ToUpper(result.Asset.String()),
		len(result.Bids),
		bidsAmount,
		result.Pair.Base,
		bidValueResult,
		len(result.Asks),
		asksAmount,
		result.Pair.Base,
		askValueResult,
	)
}

// WaitForInitialSync allows for a routine to wait for an initial sync to be
// completed without exposing the underlying type. This needs to be called in a
// separate routine.
func (m *syncManager) WaitForInitialSync() error {
	if m == nil {
		return fmt.Errorf("sync manager %w", ErrNilSubsystem)
	}

	m.inService.Wait()
	if atomic.LoadInt32(&m.started) == 0 {
		return fmt.Errorf("sync manager %w", ErrSubSystemNotStarted)
	}

	m.initSyncWG.Wait()
	return nil
}

func relayWebsocketEvent(result interface{}, event, assetType, exchangeName string) {
	evt := WebsocketEvent{
		Data:      result,
		Event:     event,
		AssetType: assetType,
		Exchange:  exchangeName,
	}
	err := BroadcastWebsocketMessage(evt)
	if !errors.Is(err, ErrWebsocketServiceNotRunning) {
		log.Errorf(log.APIServerMgr, "Failed to broadcast websocket event %v. Error: %s",
			event, err)
	}
}
