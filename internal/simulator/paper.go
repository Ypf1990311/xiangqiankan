package simulator

import (
	"fmt"
	"math"
	"sync"

	"alphahft/internal/monitor"
	"github.com/rs/zerolog/log"
)

// PaperEngine 本地前向撮合模拟器
// 通过截获实盘实时行情，在内存中模拟建仓和平仓，统计胜率与浮动盈亏（PnL）
type PaperEngine struct {
	Symbol        string
	Balance       float64 // 虚拟可用资金
	PositionAmt   float64 // 当前仓位 (+表示多头, -表示空头)
	AvgEntryPrice float64 // 开仓均价

	RealizedPnL   float64 // 累计已实现盈亏
	UnrealizedPnL float64 // 浮动盈亏

	activeBuyPrice  float64
	activeBuyQty    float64
	activeSellPrice float64
	activeSellQty   float64

	mu sync.RWMutex
}

func NewPaperEngine(symbol string, initBalance float64) *PaperEngine {
	log.Info().Str("sym", symbol).Float64("balance", initBalance).Msg("🕹️ 本地虚拟模拟撮合盘已开启！系统将拦截所有发单网络请求，在本地进行验证！")
	return &PaperEngine{
		Symbol:  symbol,
		Balance: initBalance,
	}
}

// ReplaceOrders 替代调用真实的 Binance REST API 挂单撤单接口
func (p *PaperEngine) ReplaceOrders(buyPrice, sellPrice, qty float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.activeBuyPrice = buyPrice
	p.activeBuyQty = qty

	p.activeSellPrice = sellPrice
	p.activeSellQty = qty
	return nil
}

// CancelAll 撤销本地虚假限价单
func (p *PaperEngine) CancelAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.activeBuyPrice = 0
	p.activeSellPrice = 0
	p.activeBuyQty = 0
	p.activeSellQty = 0
	return nil
}

// CheckFill 撮合核心：接受最新的 Live 成交价格进行验算
func (p *PaperEngine) CheckFill(tradePrice float64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var filledBuy, filledSell bool

	// 1. 验证多单是否被触发命中 (市场有人主动以我们的买价甚至更低卖出砸盘)
	if p.activeBuyPrice > 0 && tradePrice <= p.activeBuyPrice {
		filledBuy = true
		qty := p.activeBuyQty
		price := p.activeBuyPrice
		p.executeFill(qty, price, "BUY")
		p.activeBuyPrice = 0 // 消耗掉订单
	}

	// 2. 验证空单是否命中 (市场有人以我们的卖价甚至更高买入拉盘)
	if p.activeSellPrice > 0 && tradePrice >= p.activeSellPrice {
		filledSell = true
		qty := p.activeSellQty
		price := p.activeSellPrice
		p.executeFill(-qty, price, "SELL") // 卖单传入负数仓位
		p.activeSellPrice = 0
	}

	// 触发播报
	if filledBuy || filledSell {
		log.Info().
			Str("sym", p.Symbol).
			Float64("price_triggered", tradePrice).
			Float64("pos", p.PositionAmt).
			Float64("avg_entry", p.AvgEntryPrice).
			Str("PnL", fmt.Sprintf("%.3f", p.RealizedPnL)).
			Msg("🏁 [纸上模拟盘] 挂单被命中！执行虚拟成交")
	}

	// 用最新价格测算浮亏浮盈
	if p.PositionAmt != 0 {
		if p.PositionAmt > 0 {
			p.UnrealizedPnL = (tradePrice - p.AvgEntryPrice) * p.PositionAmt
		} else {
			p.UnrealizedPnL = (p.AvgEntryPrice - tradePrice) * math.Abs(p.PositionAmt)
		}
	} else {
		p.UnrealizedPnL = 0
	}

	// ⭐️ 资金变化实时汇报通过接口打出到三维可视化 Grafana 板上
	monitor.PromRealizedPnL.WithLabelValues(p.Symbol).Set(p.RealizedPnL)
	monitor.PromUnrealizedPnL.WithLabelValues(p.Symbol).Set(p.UnrealizedPnL)
	monitor.PromPosition.WithLabelValues(p.Symbol).Set(p.PositionAmt)
}

// executeFill 计算开平仓持仓逻辑与净值
func (p *PaperEngine) executeFill(tradeQty, tradePrice float64, side string) {
	// 如果方向一致，这是在 加仓 (例如原本就是正的，又买入了)
	if (p.PositionAmt > 0 && tradeQty > 0) || (p.PositionAmt < 0 && tradeQty < 0) || p.PositionAmt == 0 {
		newPos := p.PositionAmt + tradeQty
		// 加权平均计算新的持仓均价
		p.AvgEntryPrice = ((math.Abs(p.PositionAmt) * p.AvgEntryPrice) + (math.Abs(tradeQty) * tradePrice)) / math.Abs(newPos)
		p.PositionAmt = newPos
	} else {
		// 这是在 平仓 或 减仓 (例如持有正数时遇上了SELL卖单负数)
		remainingQty := p.PositionAmt + tradeQty
		
		// 发生了多少真实的平仓？
		closeQty := math.Min(math.Abs(p.PositionAmt), math.Abs(tradeQty))

		// 计算平仓的已实现损益 Realized PnL
		var pnl float64
		if p.PositionAmt > 0 {
			// 原本是多头
			pnl = (tradePrice - p.AvgEntryPrice) * closeQty
		} else {
			// 原本是空头
			pnl = (p.AvgEntryPrice - tradePrice) * closeQty
		}
		p.RealizedPnL += pnl

		// 判断是否仓位反转 (例如持有0.1通过卖掉-0.2，变为-0.1空头)
		if (p.PositionAmt > 0 && remainingQty < 0) || (p.PositionAmt < 0 && remainingQty > 0) {
			// 新持仓变为反向的
			p.PositionAmt = remainingQty
			p.AvgEntryPrice = tradePrice // 新方向反转后的成本默认为翻越时刻的成交价
		} else {
			// 只是减仓没有反转
			p.PositionAmt = remainingQty
			if p.PositionAmt == 0 {
				p.AvgEntryPrice = 0
			}
		}
	}
}

func (p *PaperEngine) GetState() (pos, avgPrice float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.PositionAmt, p.AvgEntryPrice
}

// GetSummary [新增] 返回一眼就能看懂的文字摘要
func (p *PaperEngine) GetSummary() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	
	status := "🟢 运行中"
	if math.Abs(p.PositionAmt) > 0.01 { // 假设仓位大了背景变黄
		status = "🟡 持仓中"
	}

	return fmt.Sprintf("[%s] 利润: %+7.3f U | 浮盈: %+7.3f U | 仓位: %+7.4f", 
		status, p.RealizedPnL, p.UnrealizedPnL, p.PositionAmt)
}

