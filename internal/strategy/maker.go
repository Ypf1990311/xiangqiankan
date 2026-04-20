package strategy

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"alphahft/config"
	"alphahft/internal/database"
	"alphahft/internal/exchange/binance"
	"alphahft/internal/orderbook"
	"alphahft/internal/risk"
	"alphahft/internal/simulator"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/rs/zerolog/log"
)

type MakerEngine struct {
	Symbol     string
	OB         *orderbook.OrderBook
	Client     *binance.FuturesClient
	Config     *config.Config
	RiskEngine *risk.RiskEngine
	VPIN       *VPINTracker
	CVD        *CVDTracker        // [新增] CVD追踪器
	LeadLag    *LeadLagTracker    // [新增] 跨品种暴跌防卫哨兵
	Paper      *simulator.PaperEngine
	Volatility *VolatilityTracker // 实时波动率估计器 (A-S 模型核心输入)
	Telemetry  *database.TelemetryDB // [新增] 数据打点黑匣子

	TargetSpread   float64 // 保留字段 (已被 SpreadFactor 替代)
	SpreadFactor   float64 // 波动率 spread 乘数：越大 spread 越宽
	InventoryGamma float64 // A-S 库存风险厌恶系数：越大越积极消化库存
	MaxPosAllowed  float64
	OrderQty       float64

	// 🎯 风控阈值参数（可通过 /api/tune 在线热修改）
	CVDFlatThreshold float64 // 空仓时 CVD 拦截鈤（默认 3.0）
	CVDPosThreshold  float64 // 持仓时 CVD 拦截鈤（默认 10.0）
	OBIThreshold     float64 // OBI 极端阈值（默认 0.8）

	mu              sync.RWMutex
	BuyStack        []database.TradeChunk
	SellStack       []database.TradeChunk
	ActiveBidID     int64
	ActiveAskID     int64
	ActiveGuardID   int64 // [新增] 持仓过重时的安全止盈/减仓护栏订单 ID
	PlacedBidPrice  float64
	PlacedAskPrice  float64
	Ledger          *database.Ledger
	TriggerChan     chan struct{}
	lastEntryTime   time.Time
	marginErrorTime time.Time // 保证金错误熔断计时器
	lastLossTime    time.Time // 上次亏损时间（亏损后冷却）
	lastHeartbeat   time.Time // 记录心跳避免刷屏
	cancelFunc      context.CancelFunc

	// 📊 订单池统计
	RoundTrips      int     // 已完成的配对数
	RoundTripProfit float64 // 所有配对的累计盈亏
}

func NewMakerEngine(symbol string, ob *orderbook.OrderBook, client *binance.FuturesClient, cfg *config.Config, riskEngine *risk.RiskEngine, ledger interface{}) *MakerEngine {
	var l *database.Ledger
	if ldg, ok := ledger.(*database.Ledger); ok {
		l = ldg
	}

	return &MakerEngine{
		Symbol:           symbol,
		OB:               ob,
		Client:           client,
		Config:           cfg,
		RiskEngine:       riskEngine,
		Ledger:           l,
		InventoryGamma:   0.5,
		MaxPosAllowed:    0.4,
		TargetSpread:     0.0001,
		SpreadFactor:     1.5,
		OrderQty:         0.011,
		CVDFlatThreshold: 3.0,  // 默认：空仓时 CVD 超过 ±3 则拦截逆势开仓
		CVDPosThreshold:  10.0, // 默认：持仓时 CVD 超过 ±10 才限制加仓
		OBIThreshold:     0.8,  // 默认： OBI 超过 0.8 禁止逆向挂单
		Volatility:       NewVolatilityTracker(0.01),
		TriggerChan:      make(chan struct{}, 15),
	}
}

func (m *MakerEngine) Start(tradeChan chan binance.UserDataEvent) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel
	if m.Ledger != nil {
		log.Info().Str("symbol", m.Symbol).Msg("🔄 正在连线交易所进行‘弹夹锚点’对账...")
		
		// 1. 等待风控引擎拿到真实的初始仓位
		for !m.RiskEngine.PositionSynced {
			time.Sleep(100 * time.Millisecond)
		}
		
		// 2. 执行锚点对账：以实盘为准清理数据库僵尸单
		m.Ledger.SyncWithExchange(m.Symbol, m.RiskEngine.PosAmt)
		
		// 3. 重新加载真正活跃的弹药
		m.BuyStack, _ = m.Ledger.LoadChunks(m.Symbol, "BUY")
		m.SellStack, _ = m.Ledger.LoadChunks(m.Symbol, "SELL")
		
		// 4. 应用最优价格排序
		sort.Slice(m.BuyStack, func(i, j int) bool {
			return m.BuyStack[i].Price > m.BuyStack[j].Price
		})
		sort.Slice(m.SellStack, func(i, j int) bool {
			return m.SellStack[i].Price < m.SellStack[j].Price
		})
		
		log.Info().Str("symbol", m.Symbol).
			Int("buys", len(m.BuyStack)).
			Int("sells", len(m.SellStack)).
			Msg("✅ 弹夹对账完成，历史记录已归档，当前活跃弹药已锁定")
	}
	m.OB.OnUpdate = func() {
		select {
		case m.TriggerChan <- struct{}{}:
		default:
		}
	}
	go m.runLoop(ctx)
	go m.listenTrades(ctx, tradeChan)
	log.Info().Str("symbol", m.Symbol).Msg("🤖 AlphaHFT V50.0 [Avellaneda-Stoikov 引擎] 启动")
}

func (m *MakerEngine) runLoop(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.TriggerChan:
			m.evaluateAndQuote()
		case <-ticker.C:
			m.evaluateAndQuote()
		}
	}
}

// evaluateAndQuote 核心决策函数 — Avellaneda-Stoikov 最优做市模型
//
// 架构分层：
//
//	第1层：多级风控 → 决定是否允许交易
//	第2层：库存审计 → 同步内部状态
//	第3层：A-S 定价 → 波动率自适应 spread + 库存偏斜 + VPIN 毒性防护
//	第4层：禁令系统 → 保证金/趋势/冷却 过滤
//	第5层：指令合成 → 最终报价与下单
func (m *MakerEngine) evaluateAndQuote() {
	// ======= [第1层：多级风控检查] =======
	riskLevel := m.RiskEngine.GetRiskLevel()
	if riskLevel == risk.RiskFrozen {
		fctx, fcancel := context.WithTimeout(context.Background(), 2*time.Second)
		m.mu.Lock()
		bidID, askID := m.ActiveBidID, m.ActiveAskID
		m.ActiveBidID, m.ActiveAskID = 0, 0
		m.mu.Unlock()
		if bidID != 0 {
			_ = m.Client.CancelOrder(fctx, m.Symbol, bidID)
		}
		if askID != 0 {
			_ = m.Client.CancelOrder(fctx, m.Symbol, askID)
		}
		fcancel()
		return
	}

	bids, asks, ts := m.OB.CopySnapshot()
	if len(bids) == 0 || len(asks) == 0 || time.Now().UnixMilli()-ts > 2000 {
		return
	}

	sig := CalculateSignals(bids, asks, 5)
	currentPos := m.RiskEngine.PosAmt
	avgEntry := m.RiskEngine.EntryPrice
	tickSize := 0.01

	// 💓 引擎心跳日志 (每 10 秒打印一次参数快照)
	if time.Since(m.lastHeartbeat) >= 10*time.Second {
		m.lastHeartbeat = time.Now()
		toxVal := 0.0
		if m.VPIN != nil { toxVal = m.VPIN.GetToxicity() }
		log.Debug().
			Str("sym", m.Symbol).
			Float64("sigma", m.Volatility.GetSigma()).
			Float64("tox", toxVal).
			Float64("pos", currentPos).
			Float64("best_entry", avgEntry). // 默认显示均价
			Msg("💓 A-S 引擎心跳参数快照")
	}

	// ======= [第2层：库存栈审计] =======
	m.mu.Lock()
	if m.RiskEngine.PositionSynced {
		m.auditStacksLocked(currentPos, avgEntry)
	}
	m.mu.Unlock()

	// ======= [第3层：Avellaneda-Stoikov 动态定价] =======

	// 3a. 实时波动率 σ (EMA 估计，价格空间)
	sigma := m.Volatility.Update(sig.MicroPrice)
	if sigma < tickSize*0.5 {
		sigma = tickSize * 0.5 // 最低波动率下限：半个 tick
	}

	// 3b. VPIN 订单流毒性感知
	toxicity := 0.0
	if m.VPIN != nil {
		toxicity = m.VPIN.GetToxicity()
	}

	// 极高毒性 → 知情交易者正在扫盘，全面撤退保护资金
	if toxicity > 0.75 {
		tctx, tcancel := context.WithTimeout(context.Background(), 2*time.Second)
		m.mu.Lock()
		bidID, askID := m.ActiveBidID, m.ActiveAskID
		m.ActiveBidID, m.ActiveAskID = 0, 0
		m.mu.Unlock()
		if bidID != 0 {
			_ = m.Client.CancelOrder(tctx, m.Symbol, bidID)
		}
		if askID != 0 {
			_ = m.Client.CancelOrder(tctx, m.Symbol, askID)
		}
		tcancel()
		return
	}

	// 3c. 基础 Spread 计算 (先于保留价格，用于限制库存偏斜)
	//
	// 最低 1 tick 保障 PostOnly 不被拒绝
	// 实际 spread 由 sigma × SpreadFactor 动态决定（BNB 通常 1.5~3 tick）
	baseHalfSpread := sigma * m.SpreadFactor
	if baseHalfSpread < tickSize {
		baseHalfSpread = tickSize
	}

	// 3d. Avellaneda-Stoikov 保留价格 — 带偏斜上限
	//
	// r = microPrice - clamp(q × γ × σ, ±maxSkew)
	//
	// 🔑 关键改进：skew 限制在 halfSpread 的 40% 以内
	// 即使满仓，exit 价格 = microPrice - 0.4*hs + hs = microPrice + 0.6*hs
	// 仍然高于当前市价 → 大多数平仓自然盈利（无需锚定成本）
	q := currentPos / m.OrderQty
	gamma := m.InventoryGamma

	// 💥 [V2 融合：三倍向心力收敛] 如果当前持仓处于盈利中，猛踩油门向内挤压拉平盈利单
	if math.Abs(currentPos) > 0.005 {
		if (currentPos > 0 && sig.MicroPrice > avgEntry) || (currentPos < 0 && sig.MicroPrice < avgEntry) {
			gamma *= 3.0
		}
	}

	rawSkew := q * gamma * sigma
	maxSkew := baseHalfSpread * 0.4 // 偏斜上限 = halfSpread 的 40%
	if rawSkew > maxSkew {
		rawSkew = maxSkew
	}
	if rawSkew < -maxSkew {
		rawSkew = -maxSkew
	}
	reservationPrice := sig.MicroPrice - rawSkew

	// 3e. 最终 Spread (叠加 VPIN 毒性防护 + 风控模式)
	halfSpread := baseHalfSpread * (1.0 + toxicity*2.0)

	// 谨慎模式：额外加宽 spread 50%
	if riskLevel == risk.RiskCautious {
		halfSpread *= 1.5
	}

	myBidPrice := reservationPrice - halfSpread
	myAskPrice := reservationPrice + halfSpread

	// 🛡️ [V2恢复] 突破 A-S 分散防线，在空仓时极限聚拢到 1 档盘口抢单
	if currentPos == 0 {
		myBidPrice = reservationPrice - tickSize*0.6
		myAskPrice = reservationPrice + tickSize*0.6
	}

	// 🛡️ 价格护栏：确保 Bid 和 Ask 之间至少有 2 tick 距离，防止同一价格挂单
	if myAskPrice - myBidPrice < tickSize*2 {
		myBidPrice -= tickSize * 0.5
		myAskPrice += tickSize * 0.5
	}

	// ======= [第4层：严格入场筛选] =======
	canBuyEntry := true
	canSellEntry := true

	// 🚨 [跨品种领跌效应拦截] BTC 动量下砸，立即停止接刀，只出不进
	if m.LeadLag != nil {
		if m.LeadLag.IsBearDanger() {
			canBuyEntry = false 
		}
		if m.LeadLag.IsBullDanger() {
			canSellEntry = false
		}
	}

	// 🚨 [量价动能与顺势拦截 (Momentum Barrier)] 
	// 分两个级别拦截，保证已开仓的盈利震荡逻辑不受影响！
	if m.CVD != nil {
		cvdNow := m.CVD.GetCVD()
		cvdFlat := m.CVDFlatThreshold
		cvdPos := m.CVDPosThreshold
		
		if currentPos == 0 {
			// 空仓开新单：使用精准的 ±CVDFlat 迗餀过滤
			if cvdNow > cvdFlat {
				canSellEntry = false // 强上涨势中空手开空 = 挡火车
				if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "BLOCK_SELL_CVD", Reason: "Strong Upward Momentum (Flat Position)"}) }
			} else if cvdNow < -cvdFlat {
				canBuyEntry = false // 强下跌势中空手开多 = 接飞刀
				if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "BLOCK_BUY_CVD", Reason: "Strong Downward Momentum (Flat Position)"}) }
			}
		} else {
			// 已持仓：盈利震荡策略不被 CVD 拦截（保持 LIFO 正常运转）
			// 仅使用较宽的 CVDPos 阠值，防止在强势走中进一步加仓
			if cvdNow > cvdPos {
				canSellEntry = false
				if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "BLOCK_SELL_CVD", Reason: "Strong Upward Momentum (Has Position)"}) }
			} else if cvdNow < -cvdPos {
				canBuyEntry = false
				if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "BLOCK_BUY_CVD", Reason: "Strong Downward Momentum (Has Position)"}) }
			}
		}

		// 2. 冰山背离陷阱探测：上方无抛压但下砸凶猛
		if sig.OBI > 0.5 && cvdNow < -(cvdPos*1.5) {
			canBuyEntry = false
			log.Info().Str("sym", m.Symbol).Msg("🕳️ 量价顶背离(OBI强/CVD弱)，熔断买单防止诱多被埋")
			if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "BLOCK_BUY_DIVERGENCE", Reason: "Divergence OBI vs CVD"}) }
		}
	}

	// 1. 保证金熔断
	if time.Since(m.marginErrorTime) < 30*time.Second {
		canBuyEntry = false
		canSellEntry = false
	}
	// 2. 🔑 盘口 spread 筛选：由于有浮点精度误差(0.01 可能变成 0.0099999) 
	// 所以用 tickSize * 0.9 容错。避免由于误差导致全部订单被砍！
	if sig.Spread < tickSize*0.9 {
		canBuyEntry = false
		canSellEntry = false
	}
	// 3. OBI 趋势拦截
	obi := m.OBIThreshold
	if sig.OBI > obi {
		canSellEntry = false // 买压极大 → 不卖
	}
	if sig.OBI < -obi {
		canBuyEntry = false // 卖压极大 → 不买
	}
	
	// [解除封印] 已撤除 1 秒强制入场冷却，允许真正的毫秒级别高频吃进！

	// 5. 亏损后冷却：防连环爆血，仅上笔真金白银止损后才冷却 3 秒
	if time.Since(m.lastLossTime) < 3*time.Second {
		canBuyEntry = false
		canSellEntry = false
	}
	// 6. 防御模式下只减仓不加仓
	if riskLevel == risk.RiskDefensive {
		if currentPos >= 0 {
			canBuyEntry = false
		}
		if currentPos <= 0 {
			canSellEntry = false
		}
	}

	// ======= [第5层：极速狙击手收割层 (V2 Fusion)] =======
	var buyTargetP, sellTargetP float64
	var buyQty, sellQty float64
	buyQty = m.OrderQty
	sellQty = m.OrderQty
	
	// 先计算当前持仓最新的那笔成本 (topEntry) 和最新的动能数据 (cvdNow)
	topEntry := avgEntry
	cvdNow := 0.0
	if m.CVD != nil { cvdNow = m.CVD.GetCVD() }

	// Taker 特权判定：如果利润极高且遇到动能反转，开启主动吃单权限保护利润
	postOnlyBuy := true
	postOnlySell := true

	if math.Abs(currentPos) > 0.005 {
		if currentPos > 0 { // 持有多头
			m.mu.Lock()
			if len(m.BuyStack) > 0 {
				topEntry = m.BuyStack[len(m.BuyStack)-1].Price
				log.Debug().
					Str("sym", m.Symbol).
					Float64("target_entry", topEntry).
					Int("stack_len", len(m.BuyStack)).
					Msg("🎯 锁定【最低入场价】订单，准备止盈离场")
			}
			m.mu.Unlock()

			isUnderwater := bids[0].Price < avgEntry - (tickSize*2) // 判断是否处于被套劣势局

			// 1. 计算市价止盈最低阈值 (手续费粗估5%%，也就是 0.0005，必须利润是其 2.5倍以上才允许吃单出逃)
			takerEmergencyThreshold := topEntry + (topEntry * 0.0005 * 2.5)
			
			// 2. 基准利润线：以最后一笔入场的成本作为底线
			profitFloor := topEntry + tickSize
			sniperPrice := bids[0].Price + tickSize

			// 🚨 [特权吃单模式]：顺风局极其盈利时，防止利润回吐
			if bids[0].Price > takerEmergencyThreshold && cvdNow < -10.0 {
				sellTargetP = bids[0].Price // 直接用买一价强行砸盘止盈
				postOnlySell = false        // 关闭挂单只做Maker模式，强行变 Taker
				if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "TAKER_RESCUE_SELL", Reason: "Securing Extreme Profit on Reversal"}) }
			} else {
				// 🚨 [保本逃生门]：逆风局被套装死时，好不容易等到了反弹，不再苛求那点网格利润，优先平本跳车脱解挂起的仓位！
				escapePrice := avgEntry - tickSize // 牺牲 1 个 tick 换取提早解脱
				if isUnderwater && sniperPrice >= escapePrice && cvdNow < -1.0 {
					sellTargetP = escapePrice
					if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "ESCAPE_HATCH_SELL", Reason: "Under-water break-even escape"}) }
				} else if sniperPrice > profitFloor {
					// 正常情况下的 Maker 狙击
					sellTargetP = sniperPrice 
				} else {
					sellTargetP = profitFloor
				}
			}

			// 💹 [被动补仓阵线]
			if canBuyEntry && currentPos < m.MaxPosAllowed && sig.MicroPrice < topEntry - (tickSize*2) {
				// 🚨 [防飞刀屏障]：如果处于被套中，但是天上正在下刀子(大瀑布)，绝对装死不补多单！
				if isUnderwater && cvdNow < -10.0 {
					if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "BLOCK_KNIFE_BUY", Reason: "CVD Freefall, paused DCA"}) }
				} else {
					buyTargetP = myBidPrice
				}
			}
		} else { // 持有空头
			m.mu.Lock()
			if len(m.SellStack) > 0 {
				topEntry = m.SellStack[len(m.SellStack)-1].Price
				log.Debug().
					Str("sym", m.Symbol).
					Float64("target_entry", topEntry).
					Int("stack_len", len(m.SellStack)).
					Msg("🎯 锁定【最高入场价】订单，准备买回平单")
			}
			m.mu.Unlock()

			isUnderwater := asks[0].Price > avgEntry + (tickSize*2)

			// 计算吃单出局特权阈值
			takerEmergencyThreshold := topEntry - (topEntry * 0.0005 * 2.5)

			profitFloor := topEntry - tickSize
			sniperPrice := asks[0].Price - tickSize
			
			// 🚨 [特权吃单模式]
			if asks[0].Price < takerEmergencyThreshold && cvdNow > 10.0 {
				buyTargetP = asks[0].Price // 直接用卖一价强吃
				postOnlyBuy = false
				if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "TAKER_RESCUE_BUY", Reason: "Securing Extreme Profit on Reversal"}) }
			} else {
				escapePrice := avgEntry + tickSize 
				if isUnderwater && sniperPrice <= escapePrice && cvdNow > 1.0 {
					buyTargetP = escapePrice
					if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "ESCAPE_HATCH_BUY", Reason: "Under-water break-even escape"}) }
				} else if sniperPrice < profitFloor {
					buyTargetP = sniperPrice
				} else {
					buyTargetP = profitFloor
				}
			}

			// 💹 补空网格阵线
			if canSellEntry && math.Abs(currentPos) < m.MaxPosAllowed && sig.MicroPrice > topEntry + (tickSize*2) {
				if isUnderwater && cvdNow > 10.0 {
					if m.Telemetry != nil { m.Telemetry.PushSignal(database.SignalData{Timestamp: time.Now().UnixMilli(), Symbol: m.Symbol, Action: "BLOCK_KNIFE_SELL", Reason: "CVD Rocket, paused DCA"}) }
				} else {
					sellTargetP = myAskPrice
				}
			}
		}
	} else {
		// ===== 核心空仓态：超窄极速双边下饵铺单 =====
		if canBuyEntry { buyTargetP = myBidPrice }
		if canSellEntry { sellTargetP = myAskPrice }
	}

	// 价格护栏：确保不穿过盘口 (PostOnly/GTX 安全)
	if buyTargetP >= bids[0].Price && len(bids) > 0 {
		buyTargetP = bids[0].Price
	}
	if sellTargetP <= asks[0].Price && len(asks) > 0 {
		sellTargetP = asks[0].Price
	}

	// ⭐️ [写入遥测数据库] 丢出高频快照
	if m.Telemetry != nil {
		m.Telemetry.PushTick(database.TickData{
			Timestamp: time.Now().UnixMilli(),
			Symbol: m.Symbol,
			MicroPrice: sig.MicroPrice,
			Spread: sig.Spread,
			OBI: sig.OBI,
			CVD: cvdNow,
			VPIN: toxicity,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	m.mu.Lock()
	lbid, lask := m.ActiveBidID, m.ActiveAskID
	lbidP, laskP := m.PlacedBidPrice, m.PlacedAskPrice
	m.mu.Unlock()

	// 买单执行
	replaceBuy := false
	if buyTargetP > 0 {
		if lbid == 0 {
			replaceBuy = true
		} else {
			distToTop := math.Abs(lbidP - bids[0].Price)
			if postOnlyBuy && currentPos < 0 && lbidP <= topEntry && distToTop <= tickSize*2 {
				replaceBuy = false
			} else if math.Abs(buyTargetP-lbidP) >= tickSize*0.4 || !postOnlyBuy {
				replaceBuy = true
			}
		}
	}

	if replaceBuy {
		if lbid != 0 {
			_ = m.Client.CancelOrder(ctx, m.Symbol, lbid)
		}
		id, err := m.Client.PlaceMakerOrder(ctx, m.Symbol, futures.SideTypeBuy, buyTargetP, buyQty, 2, 2, postOnlyBuy)
		if err == nil && id != 0 {
			m.mu.Lock()
			m.ActiveBidID = id
			m.PlacedBidPrice = buyTargetP
			m.mu.Unlock()
			if buyQty == m.OrderQty {
				m.lastEntryTime = time.Now()
			}
			log.Debug().Float64("price", buyTargetP).Msg("✅ 已挂买单")
		} else if err != nil {
			errMsg := err.Error()
			if strings.Contains(errMsg, "-2019") || strings.Contains(errMsg, "insufficient") {
				m.marginErrorTime = time.Now()
				log.Warn().Msg("🚨 检测到保证金不足，开仓指令已熔断禁用 30s")
			}
		}
	} else if buyTargetP == 0 && lbid != 0 {
		_ = m.Client.CancelOrder(ctx, m.Symbol, lbid)
		m.mu.Lock()
		m.ActiveBidID = 0
		m.mu.Unlock()
	}

	// 卖单执行
	replaceSell := false
	if sellTargetP > 0 {
		if lask == 0 {
			replaceSell = true
		} else {
			distToTop := math.Abs(laskP - asks[0].Price)
			if postOnlySell && currentPos > 0 && laskP >= topEntry && distToTop <= tickSize*2 {
				replaceSell = false
			} else if math.Abs(sellTargetP-laskP) >= tickSize*0.4 || !postOnlySell {
				replaceSell = true
			}
		}
	}

	if replaceSell {
		if lask != 0 {
			_ = m.Client.CancelOrder(ctx, m.Symbol, lask)
		}
		id, err := m.Client.PlaceMakerOrder(ctx, m.Symbol, futures.SideTypeSell, sellTargetP, sellQty, 2, 2, postOnlySell)
		if err == nil && id != 0 {
			m.mu.Lock()
			m.ActiveAskID = id
			m.PlacedAskPrice = sellTargetP
			m.mu.Unlock()
			if sellQty == m.OrderQty {
				m.lastEntryTime = time.Now()
			}
			log.Debug().Float64("price", sellTargetP).Msg("✅ 已挂卖单")
		} else if err != nil {
			errMsg := err.Error()
			if strings.Contains(errMsg, "-2019") || strings.Contains(errMsg, "insufficient") {
				m.marginErrorTime = time.Now()
				log.Warn().Msg("🚨 检测到保证金不足，开仓指令已熔断禁用 30s")
			}
		}
	} else if sellTargetP == 0 && lask != 0 {
		_ = m.Client.CancelOrder(ctx, m.Symbol, lask)
		m.mu.Lock()
		m.ActiveAskID = 0
		m.mu.Unlock()
	}

	// 🛡️ [新增] 安全减仓护栏 (Guard TP)：当仓位 > 80% 时，强制挂出一个保本止盈单
	m.handleGuardTP(currentPos, avgEntry, sig.MicroPrice)
}

func (m *MakerEngine) listenTrades(ctx context.Context, ch chan binance.UserDataEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case trade := <-ch:
			if trade.Order.Symbol == m.Symbol && (trade.Order.Status == "FILLED" || trade.Order.Status == "PARTIALLY_FILLED") {
				m.handleTradeEvent(trade)
			}
		}
	}
}

func (m *MakerEngine) handleTradeEvent(event binance.UserDataEvent) {
	price, _ := strconv.ParseFloat(event.Order.LastFillPrice, 64)
	qty, _ := strconv.ParseFloat(event.Order.LastFillQty, 64)
	m.mu.Lock()
	defer m.mu.Unlock()
	if event.Order.Side == "BUY" {
		if len(m.SellStack) > 0 {
			// 📊 平空配对关闭：买回价 vs 卖出价
			entryPrice := m.SellStack[len(m.SellStack)-1].Price
			pairPnL := (entryPrice - price) * qty
			m.RoundTrips++
			m.RoundTripProfit += pairPnL
			if pairPnL < 0 {
				m.lastLossTime = time.Now()
			}
			log.Info().
				Float64("entry", entryPrice).
				Float64("exit", price).
				Float64("pnl", pairPnL).
				Int("total_pairs", m.RoundTrips).
				Float64("total_pnl", m.RoundTripProfit).
				Msg("🔄 配对关闭 [空→买回]")
			if m.Ledger != nil {
				_ = m.Ledger.SaveFill(m.Symbol, "BUY", price, qty, pairPnL)
			}
			m.popStackLocked(&m.SellStack, qty, "SELL")
		} else {
			m.BuyStack = append(m.BuyStack, database.TradeChunk{Price: price, Qty: qty})
			// 📌 按价格降序排列，栃顶始终是最低买入价（利润最大的那笔）
			sort.Slice(m.BuyStack, func(i, j int) bool {
				return m.BuyStack[i].Price > m.BuyStack[j].Price // 降序：高价在前，低价在尾（pop端）
			})
			log.Info().
				Float64("price", price).Float64("qty", qty).
				Int("pool_size", len(m.BuyStack)).
				Float64("top_entry", m.BuyStack[len(m.BuyStack)-1].Price).
				Msg("⭕ 订单池 [买入新增 | 最优入场价配寻止盈]")
			if m.Ledger != nil {
				m.Ledger.SaveChunk(m.Symbol, "BUY", price, qty)
				_ = m.Ledger.SaveFill(m.Symbol, "BUY", price, qty, 0)
			}
		}
	} else {
		if len(m.BuyStack) > 0 {
			// 📊 平多配对关闭：卖出价 vs 买入价
			entryPrice := m.BuyStack[len(m.BuyStack)-1].Price
			pairPnL := (price - entryPrice) * qty
			m.RoundTrips++
			m.RoundTripProfit += pairPnL
			if pairPnL < 0 {
				m.lastLossTime = time.Now()
			}
			log.Info().
				Float64("entry", entryPrice).
				Float64("exit", price).
				Float64("pnl", pairPnL).
				Int("total_pairs", m.RoundTrips).
				Float64("total_pnl", m.RoundTripProfit).
				Msg("🔄 配对关闭 [多→卖出]")
			if m.Ledger != nil {
				_ = m.Ledger.SaveFill(m.Symbol, "SELL", price, qty, pairPnL)
			}
			m.popStackLocked(&m.BuyStack, qty, "BUY")
		} else {
			m.SellStack = append(m.SellStack, database.TradeChunk{Price: price, Qty: qty})
			// 📌 按价格升序排列，栃顶始终是最高卖出价（利润最大的那笔）
			sort.Slice(m.SellStack, func(i, j int) bool {
				return m.SellStack[i].Price < m.SellStack[j].Price // 升序：低价在前，高价在尾（pop端）
			})
			log.Info().
				Float64("price", price).Float64("qty", qty).
				Int("pool_size", len(m.SellStack)).
				Float64("top_entry", m.SellStack[len(m.SellStack)-1].Price).
				Msg("🔴 订单池 [卖出新增 | 最优卖出价配寻止盈]")
			if m.Ledger != nil {
				m.Ledger.SaveChunk(m.Symbol, "SELL", price, qty)
				_ = m.Ledger.SaveFill(m.Symbol, "SELL", price, qty, 0)
			}
		}
	}

	// 🛡️ [新增] 如果成交的是安全护栏订单，重置 GuardID
	if event.Order.ID == m.ActiveGuardID {
		if event.Order.Status == "FILLED" {
			m.ActiveGuardID = 0
			log.Info().Str("sym", m.Symbol).Msg("🛡️ 安全护栏订单已完全成交，风险已释放")
		}
	}
}

func (m *MakerEngine) auditStacksLocked(currentPos float64, avgPrice float64) {
	if math.Abs(currentPos) < 0.005 {
		m.BuyStack = nil
		m.SellStack = nil
		return
	}

	stackQty := 0.0
	isLong := currentPos > 0
	
	if isLong {
		for _, c := range m.BuyStack { stackQty += c.Qty }
		if math.Abs(currentPos-stackQty) > 0.05 {
			// ❌ 不再直接“抹平”成均价单！
			// ✅ 尝试从数据库重新加载带有梯度的记录
			dbChunks, err := m.Ledger.LoadChunks(m.Symbol, "BUY")
			if err == nil && len(dbChunks) > 0 {
				m.BuyStack = dbChunks
				log.Debug().Str("sym", m.Symbol).Msg("🔄 [审计] 内存弹夹已通过数据库梯度完成同步(非抹平)")
			} else {
				// 数据库也没了，被迫保命：使用均价
				m.BuyStack = []database.TradeChunk{{Price: avgPrice, Qty: currentPos}}
				log.Warn().Str("sym", m.Symbol).Msg("🚨 [审计] 数据库同步失败，被迫使用均价备份")
			}
		}
	} else {
		for _, c := range m.SellStack { stackQty += c.Qty }
		if math.Abs(math.Abs(currentPos)-stackQty) > 0.05 {
			dbChunks, err := m.Ledger.LoadChunks(m.Symbol, "SELL")
			if err == nil && len(dbChunks) > 0 {
				m.SellStack = dbChunks
				log.Debug().Str("sym", m.Symbol).Msg("🔄 [审计] 内存弹夹已通过数据库梯度完成同步(非抹平)")
			} else {
				m.SellStack = []database.TradeChunk{{Price: avgPrice, Qty: math.Abs(currentPos)}}
				log.Warn().Str("sym", m.Symbol).Msg("🚨 [审计] 数据库同步失败，被迫使用均价备份")
			}
		}
	}
}

// handleGuardTP 处理高仓位时刻的安全止盈护栏
func (m *MakerEngine) handleGuardTP(currentPos, avgEntry, microPrice float64) {
	threshold := m.MaxPosAllowed * 0.8
	if math.Abs(currentPos) < threshold {
		// 境内安全：如果有正在挂着的护栏，撤销它
		m.mu.Lock()
		gid := m.ActiveGuardID
		m.ActiveGuardID = 0
		m.mu.Unlock()
		if gid != 0 {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = m.Client.CancelOrder(ctx, m.Symbol, gid)
				log.Info().Str("sym", m.Symbol).Msg("🛡️ 仓位回落安全区，撤销安全护栏订单")
			}()
		}
		return
	}

	// 境外警戒：正在面临大幅回撤风险，尝试挂出护栏
	m.mu.Lock()
	if m.ActiveGuardID != 0 {
		m.mu.Unlock()
		return // 已经有护栏在守着了
	}
	m.mu.Unlock()

	// 决定是市价平一半，还是挂单平一半
	qty := math.Abs(currentPos) * 0.5
	pricePrecision := 2
	qtyPrecision := 2 // BNB 精度

	// 计算止盈目标价 (覆盖 单边手续费 + 0.2 USDT 订单净利润)
	// 用户提供的手续费率为 0.001 (0.1%)
	feeBuffer := avgEntry * 0.001
	// 0.2 是整笔订单的利润目标，折算到单价：
	profitBuffer := 0.2 / qty

	targetPrice := 0.0
	var side futures.SideType

	isLong := currentPos > 0
	if isLong {
		targetPrice = avgEntry + feeBuffer + profitBuffer
		side = futures.SideTypeSell
	} else {
		targetPrice = avgEntry - feeBuffer - profitBuffer
		side = futures.SideTypeBuy
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var orderID int64
	var err error

	if (isLong && microPrice >= targetPrice) || (!isLong && microPrice <= targetPrice) {
		// 现价已经达标：直接市价吃单 (Taker) 锁定利润
		log.Warn().Str("sym", m.Symbol).Float64("pos", currentPos).Msg("🛡️ 仓位过重且利润达标，立即执行【市价】止盈减仓(50%)")
		orderID, err = m.Client.PlaceMarketOrder(ctx, m.Symbol, side, qty, qtyPrecision)
	} else {
		// 价格还没到：在目标位挂限价单 (Limit GTC) 守株待兔
		log.Info().Str("sym", m.Symbol).Float64("target", targetPrice).Msg("🛡️ 仓位过重，正在目标价挂设【限价】止盈护栏(50%)")
		orderID, err = m.Client.PlaceMakerOrder(ctx, m.Symbol, side, targetPrice, qty, pricePrecision, qtyPrecision, false)
	}

	if err == nil {
		m.mu.Lock()
		m.ActiveGuardID = orderID
		m.mu.Unlock()
	}
}

func (m *MakerEngine) popStackLocked(stack *[]database.TradeChunk, qty float64, targetSide string) {

	if m.Ledger != nil {
		m.Ledger.PopChunk(m.Symbol, targetSide, qty)
	}
	remaining := qty
	for remaining > 0 && len(*stack) > 0 {
		topIdx := len(*stack) - 1
		top := (*stack)[topIdx]
		if top.Qty <= remaining+1e-7 {
			remaining -= top.Qty
			*stack = (*stack)[:topIdx]
		} else {
			(*stack)[topIdx].Qty -= remaining
			remaining = 0
		}
	}
}

func (m *MakerEngine) Stop() {
	if m.cancelFunc != nil {
		m.cancelFunc()
	}
	log.Info().Msg("🛑 Avellaneda-Stoikov 引擎已安全停止")
}
