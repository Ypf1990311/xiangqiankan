package risk

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"alphahft/internal/exchange/binance"
	"alphahft/internal/notify"

	"github.com/rs/zerolog/log"
)

// RiskLevel 多级风控状态
type RiskLevel int

const (
	RiskNormal    RiskLevel = iota // 正常做市
	RiskCautious                   // 谨慎：加宽 spread
	RiskDefensive                  // 防御：只平仓不开仓
	RiskFrozen                     // 冻结：全部撤单
)

type RiskEngine struct {
	Client *binance.FuturesClient
	Symbol string

	// 缓存本系统获取的真实账户风控状态
	PosAmt           float64
	PositionSynced   bool    // 👈 [新增] 标记启动后是否已拿到真实的初始真实仓位
	EntryPrice       float64 // 👈 [新增] 实盘平均入场价
	Unrealized       float64
	LiquidationPrice float64
	MarkPrice        float64
	FundingRate      float64

	// 🛡️ [递进式风控]
	MaxDailyLoss     float64 // 每日最大允许亏损 (绝对值, 0=不限)

	// ⭐️ [本次运行统计]
	InitialEquity  float64 // 启动瞬间的总净值
	SessionProfit  float64 // 本次运行累计盈亏

	Notifier         *notify.TelegramNotifier
	mu sync.RWMutex
}

func NewRiskEngine(client *binance.FuturesClient, symbol string) *RiskEngine {
	return &RiskEngine{
		Client: client,
		Symbol: symbol,
	}
}

// StartRoutine 启动一个异步巡逻逻辑，每秒向币安请求一次当前该交易对的真实爆仓价和持仓量
func (r *RiskEngine) StartRoutine() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			if r.Client.Client == nil {
				continue
			}
			
			pos, err := r.Client.GetPositionRisk(context.Background(), r.Symbol)
			if err != nil {
				continue
			}

			posAmt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
			liqPrice, _ := strconv.ParseFloat(pos.LiquidationPrice, 64)
			markPrice, _ := strconv.ParseFloat(pos.MarkPrice, 64)
			unrealized, _ := strconv.ParseFloat(pos.UnRealizedProfit, 64)
			entryPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64) // 👈 抓取真实成本
			
			// 并发获取一次资金费率
			fRate, _ := r.Client.GetFundingRate(context.Background(), r.Symbol)

			r.mu.Lock()
			// [V50.0] 混合对账机制：WebSocket 依然是第一优先级，但 REST 轮询作为“真理兜底”
			// 如果两者偏差过大 (>1%) 或数据库刚开始初始化，则允许 REST 强行修正
			if !r.PositionSynced || math.Abs(r.PosAmt-posAmt) > 0.0001 || math.Abs(r.EntryPrice-entryPrice) > 0.1 {
				r.PosAmt = posAmt
				r.EntryPrice = entryPrice
				if !r.PositionSynced {
					log.Info().Str("sym", r.Symbol).Float64("pos", posAmt).Float64("entry", entryPrice).Msg("🔄 [风险核心] 初始仓位已从实盘同步成功")
				}
			}
			r.Unrealized = unrealized
			r.LiquidationPrice = liqPrice
			r.MarkPrice = markPrice
			r.FundingRate = fRate
			r.PositionSynced = true 

			// 计算当前总净值 (钱包余额 + 浮盈)
			acc, err := r.Client.Client.NewGetAccountService().Do(context.Background())
			if err == nil {
				totalBalance, _ := strconv.ParseFloat(acc.TotalMarginBalance, 64)
				currentEquity := totalBalance

				// 如果是第一次运行，记录起跑线
				if r.InitialEquity == 0 {
					r.InitialEquity = currentEquity
					log.Info().Float64("start_equity", r.InitialEquity).Msg("🚩 已锁定本次运行起跑线净值")
				}
				r.SessionProfit = currentEquity - r.InitialEquity
			}

			
			r.mu.Unlock()

		}
	}()
	log.Info().Str("symbol", r.Symbol).Msg("🛡️ 底层爆仓风控监控器已启动 (Polling)")
}

// PassesSafetyCheck 综合风控决策入口
func (r *RiskEngine) PassesSafetyCheck() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 1. 如果没有仓位或者标记价格为异常，允许进行开盲单
	if r.LiquidationPrice == 0 || r.MarkPrice == 0 || r.PosAmt == 0 {
		return true
	}

	// 2. 距离爆仓距离 (差价 / 标记价 的绝对值)
	distanceToLiq := math.Abs(r.MarkPrice-r.LiquidationPrice) / r.MarkPrice
	if distanceToLiq < 0.05 { // 5% 的距离熔断
		log.Error().Float64("distance_ratio", distanceToLiq).Msg("🚨 距爆仓价极度危险(小于5%)，风控核心冻结新挂单！")
		
		if r.Notifier != nil {
			r.Notifier.Send(fmt.Sprintf("🚨 <b>爆仓警告熔断触发</b>\n交易对: %s \n距离爆仓空间: 仅剩 %.2f%%\n强平价: %.2f | 标记价: %.2f\n<i>已切断造市单向流动性输出！</i>", r.Symbol, distanceToLiq*100, r.LiquidationPrice, r.MarkPrice))
		}
		
		return false
	}

	// 本层不再直接执行持仓上限硬性死锁，而交由 Maker 层分离判断单边方向
	return true
}

// GetRiskLevel 返回多层级风控状态
// 替代简单的 bool，提供 Normal → Cautious → Defensive → Frozen 四个递进层级
func (r *RiskEngine) GetRiskLevel() RiskLevel {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 第1层：每日亏损止损 (最高优先级)
	if r.MaxDailyLoss > 0 && r.SessionProfit < -r.MaxDailyLoss {
		return RiskFrozen
	}

	// 第2层：无仓位或无标记价 → 正常交易
	if r.LiquidationPrice == 0 || r.MarkPrice == 0 || r.PosAmt == 0 {
		return RiskNormal
	}

	// 第3层：爆仓距离递进式控制
	distanceToLiq := math.Abs(r.MarkPrice-r.LiquidationPrice) / r.MarkPrice
	if distanceToLiq < 0.05 {
		return RiskFrozen // 5% 内：全部冻结
	}
	if distanceToLiq < 0.10 {
		return RiskDefensive // 10% 内：只允许平仓
	}
	if distanceToLiq < 0.20 {
		return RiskCautious // 20% 内：加宽 spread
	}

	return RiskNormal
}

// UpdatePosition 由 WebSocket ACCOUNT_UPDATE 实时推送仓位更新
// 延迟从 REST 轮询的 2 秒降至 <50ms
func (r *RiskEngine) UpdatePosition(posAmt, entryPrice, unrealized float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.PosAmt = posAmt
	r.EntryPrice = entryPrice
	r.Unrealized = unrealized
	r.PositionSynced = true
}

func (r *RiskEngine) GetFundingRate() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.FundingRate
}

// [V50.0] 获取仓位快照，确保外部（如日志）读取时是线程安全的
func (r *RiskEngine) GetPositionSnapshot() (float64, float64, float64, float64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.PosAmt, r.EntryPrice, r.Unrealized, r.MarkPrice
}

func (r *RiskEngine) GetSessionStats() (float64, float64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.InitialEquity, r.SessionProfit
}

