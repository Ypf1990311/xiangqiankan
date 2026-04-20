package strategy

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/rs/zerolog/log"
)

// LeadLagTracker 跨品种（大饼 BTC）领跑预警雷达
type LeadLagTracker struct {
	LeaderSymbol string
	DropThreshold float64 // 比如 -30.0，代表1秒内 BTC 暴跌超 30 刀即触发熔断防做多
	SurgeThreshold float64 // 比如 30.0，代表1秒内 BTC 暴涨超 30 刀即触发熔断防做空
	mu sync.RWMutex
	lastPrice float64
	alertBearTime time.Time
	alertBullTime time.Time
}

func NewLeadLagTracker(leaderSymbol string, dropThreshold float64, surgeThreshold float64) *LeadLagTracker {
	return &LeadLagTracker{
		LeaderSymbol:  leaderSymbol,
		DropThreshold: dropThreshold,
		SurgeThreshold: surgeThreshold,
	}
}

func (l *LeadLagTracker) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_, stopC, err := futures.WsAggTradeServe(l.LeaderSymbol, func(event *futures.WsAggTradeEvent) {
					price, _ := strconv.ParseFloat(event.Price, 64)
					l.mu.Lock()
					if l.lastPrice == 0 {
						l.lastPrice = price
					} else {
						// 简单的动能判定：如果当前价格比上一帧低了非常多
						diff := price - l.lastPrice
						if diff < l.DropThreshold { // 跌超阈值
							l.alertBearTime = time.Now()
							log.Warn().
								Str("leader", l.LeaderSymbol).
								Float64("drop", diff).
								Msg("🚨 [跨品种雷达] 检测到大饼剧烈下砸，屏蔽下方接刀买单！")
						} else if diff > l.SurgeThreshold { // 涨超阈值
							l.alertBullTime = time.Now()
							log.Warn().
								Str("leader", l.LeaderSymbol).
								Float64("surge", diff).
								Msg("🚀 [跨品种雷达] 检测到大饼剧烈火箭起飞，屏蔽上方挡车卖单！")
						}
						// 收敛更新基准价 (使用缓慢滑动避免轻易被极值影响)
						l.lastPrice = l.lastPrice*0.9 + price*0.1 
					}
					l.mu.Unlock()
				}, func(err error) {
					log.Warn().Err(err).Msg("Leader stream error")
				})
				
				if err != nil {
					time.Sleep(3 * time.Second)
					continue
				}
				
				<-ctx.Done()
				if stopC != nil {
					close(stopC)
				}
				return
			}
		}
	}()
	log.Info().Str("leader", l.LeaderSymbol).Msg("📡 [全网监控] BTC领头羊异动哨击雷达已开启")
}

// IsBearDanger 返回由于大盘抛售带来的做多危险状态
func (l *LeadLagTracker) IsBearDanger() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	// 雷达预警有效期：大盘暴跌后 3 秒内，小币种禁止接飞刀
	return time.Since(l.alertBearTime) < 3*time.Second 
}

// IsBullDanger 返回由于大盘爆拉带来的做空危险状态
func (l *LeadLagTracker) IsBullDanger() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	// 雷达预警有效期：大盘暴涨后 3 秒内，小币种禁止挡火车
	return time.Since(l.alertBullTime) < 3*time.Second 
}
