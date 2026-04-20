package binance

import (
	"math"
	"strconv"
	"time"
	
	"alphahft/internal/orderbook"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/rs/zerolog/log"
)

// MarketStream 负责接收并拆解市场 WebSocket 数据
type MarketStream struct {
	Symbol     string
	OrderBook  *orderbook.OrderBook
	stopC      chan struct{}   // 停止信号信道
	doneC      chan struct{}   // 已断开信号
}

func NewMarketStream(symbol string, ob *orderbook.OrderBook) *MarketStream {
	return &MarketStream{
		Symbol:    symbol,
		OrderBook: ob,
		stopC:     make(chan struct{}),
		doneC:     make(chan struct{}),
	}
}

// Start 启动对目标合约的 20档 100ms 深度快照订阅
func (m *MarketStream) Start() {
	go func() {
		retryCount := 0
		for {
			wsHandler := func(event *futures.WsDepthEvent) {
				bids := make([]orderbook.Level, 0, len(event.Bids))
				for _, bid := range event.Bids {
					price, _ := strconv.ParseFloat(bid.Price, 64)
					qty, _ := strconv.ParseFloat(bid.Quantity, 64)
					bids = append(bids, orderbook.Level{Price: price, Qty: qty})
				}
				asks := make([]orderbook.Level, 0, len(event.Asks))
				for _, ask := range event.Asks {
					price, _ := strconv.ParseFloat(ask.Price, 64)
					qty, _ := strconv.ParseFloat(ask.Quantity, 64)
					asks = append(asks, orderbook.Level{Price: price, Qty: qty})
				}
				m.OrderBook.UpdateSnapshot(bids, asks, event.Time)
			}

			errHandler := func(err error) {
				log.Error().Err(err).Str("symbol", m.Symbol).Msg("⚠️ 行情 WebSocket 断开，准备重连...")
			}

			levels := 20
			doneC, stopC, err := futures.WsPartialDepthServeWithRate(m.Symbol, levels, 100*time.Millisecond, wsHandler, errHandler)
			if err != nil {
				retryCount++
				waitSec := time.Duration(math.Min(float64(retryCount*2), 30)) // 最长等30秒
				log.Warn().Err(err).Msgf("❌ 行情连接建立失败，%v后进行第 %d 次重试...", waitSec*time.Second, retryCount)
				time.Sleep(waitSec * time.Second)
				continue
			}

			retryCount = 0
			m.stopC = stopC
			m.doneC = doneC
			log.Info().Str("symbol", m.Symbol).Msg("✔️ 高频订单簿数据流(100ms)监听已启动...")
			<-m.doneC // 阻塞直到连接断开
			time.Sleep(1 * time.Second)
		}
	}()
}

// Stop 停止订阅
func (m *MarketStream) Stop() {
	close(m.stopC)
	<-m.doneC
	log.Info().Str("symbol", m.Symbol).Msg("🛑 行情流已关闭")
}
