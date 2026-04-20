package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rs/zerolog/log"
)

type UserDataEvent struct {
	Event  string `json:"e"` // "ORDER_TRADE_UPDATE"
	Order  struct {
		Symbol              string `json:"s"`
		Side                string `json:"S"`
		Price               string `json:"p"`
		Qty                 string `json:"q"`
		ID                  int64  `json:"i"` // [新增] 订单 ID
		LastFillQty         string `json:"l"`
		LastFillPrice       string `json:"L"`
		ExecutionType       string `json:"x"`
		Status              string `json:"X"`
		CumulativeFilledQty string `json:"z"`
	} `json:"o"`
}

// PositionUpdate 实时仓位变更推送数据 (来自 ACCOUNT_UPDATE 事件)
type PositionUpdate struct {
	Symbol     string
	PosAmt     float64
	EntryPrice float64
	Unrealized float64
}

// 辅助转换方法：获取格式化后的 Symbol 用于过滤
func (u *UserDataEvent) GetSymbol() string { return u.Order.Symbol }

type UserStream struct {
	client    *FuturesClient
	listenKey string
	stopC     chan struct{}
	TradeChan    chan UserDataEvent  // 将成交消息传递给策略引擎
	PositionChan chan PositionUpdate  // 🚀 实时仓位变更推送 (ACCOUNT_UPDATE)
}

func NewUserStream(client *FuturesClient) *UserStream {
	return &UserStream{
		client:       client,
		stopC:        make(chan struct{}),
		TradeChan:    make(chan UserDataEvent, 100),
		PositionChan: make(chan PositionUpdate, 100),
	}
}

func (s *UserStream) Start(ctx context.Context) error {
	// 1. 获取 ListenKey
	listenKey, err := s.client.Client.NewStartUserStreamService().Do(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("📡 私有流 ListenKey 获取失败，系统将延迟重试")
		go s.retryListenKey()
		return nil
	}
	s.listenKey = listenKey
	log.Info().Msg("📡 私有流 ListenKey 获取成功")

	// 2. 定期续期 ListenKey (每 30 分钟一次)
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		for {
			select {
			case <-ticker.C:
				_ = s.client.Client.NewKeepaliveUserStreamService().ListenKey(s.listenKey).Do(context.Background())
			case <-s.stopC:
				return
			}
		}
	}()

	// 3. 建立 Websocket 连接
	go s.connectAndRead()
	return nil
}

func (s *UserStream) connectAndRead() {
	url := fmt.Sprintf("wss://fstream.binance.com/ws/%s", s.listenKey)
	log.Info().Msg("📡 正在连接币安私有数据流...")

	for {
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			log.Error().Err(err).Msg("❌ 私有流连接失败，3秒后重试")
			time.Sleep(3 * time.Second)
			continue
		}

		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Warn().Msg("⚠️ 私有流断开，正在重连...")
				break
			}

			var event map[string]interface{}
			json.Unmarshal(message, &event)

			// 🚀 核心解析逻辑
			if event["e"] == "ORDER_TRADE_UPDATE" {
				var tradeEvent UserDataEvent
				if err := json.Unmarshal(message, &tradeEvent); err == nil {
					log.Info().
						Str("sym", tradeEvent.GetSymbol()).
						Str("side", tradeEvent.Order.Side).
						Str("fill", tradeEvent.Order.LastFillQty).
						Msg("🔥 捕获到真实成交记录")
					s.TradeChan <- tradeEvent
				}
			}

			// 🚀 解析实时仓位变更 (比 REST 轮询快 50-100 倍)
			if event["e"] == "ACCOUNT_UPDATE" {
				var raw struct {
					Account struct {
						Positions []struct {
							Symbol     string `json:"s"`
							PosAmt     string `json:"pa"`
							EntryPrice string `json:"ep"`
							Unrealized string `json:"up"`
						} `json:"P"`
					} `json:"a"`
				}
				if err := json.Unmarshal(message, &raw); err == nil {
					for _, pos := range raw.Account.Positions {
						posAmt, _ := strconv.ParseFloat(pos.PosAmt, 64)
						entryPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64)
						unrealized, _ := strconv.ParseFloat(pos.Unrealized, 64)
						select {
						case s.PositionChan <- PositionUpdate{
							Symbol:     pos.Symbol,
							PosAmt:     posAmt,
							EntryPrice: entryPrice,
							Unrealized: unrealized,
						}:
						default:
						}
					}
				}
			}
		}
		conn.Close()
		select {
		case <-s.stopC:
			return
		default:
		}
	}
}

func (s *UserStream) retryListenKey() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			listenKey, err := s.client.Client.NewStartUserStreamService().Do(context.Background())
			if err == nil {
				s.listenKey = listenKey
				log.Info().Msg("📡 私有流 ListenKey 重新获取成功，正在重新连接...")
				go s.connectAndRead()
				return
			}
		case <-s.stopC:
			return
		}
	}
}

func (s *UserStream) Stop() {
	close(s.stopC)
}
