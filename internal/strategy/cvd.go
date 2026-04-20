package strategy

import (
	"sync"
	"time"
)

// TradeItem 记录单笔主动买卖信息
type TradeItem struct {
	Timestamp time.Time
	Volume    float64 // 净买入为正，净卖出为负
}

// CVDTracker 主动买卖累计差追踪器 (Cumulative Volume Delta)
type CVDTracker struct {
	mu          sync.RWMutex
	duration    time.Duration
	trades      []TradeItem
	currentCVD  float64
}

func NewCVDTracker(window time.Duration) *CVDTracker {
	return &CVDTracker{
		duration: window,
		trades:   make([]TradeItem, 0, 1000),
	}
}

// AddTrade 投喂归集交易数据 (isBuyerMaker = true 说明吃单方是卖方，带来下跌动能)
func (c *CVDTracker) AddTrade(isBuyerMaker bool, qty float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	vol := qty
	if isBuyerMaker {
		vol = -qty // 主动卖穿
	}

	c.trades = append(c.trades, TradeItem{Timestamp: time.Now(), Volume: vol})
	c.currentCVD += vol
}

// GetCVD 暴露当前时间窗口内的累计主动吃单差额
func (c *CVDTracker) GetCVD() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := time.Now().Add(-c.duration)
	
	// 剔除过期的记录
	idx := 0
	for ; idx < len(c.trades); idx++ {
		if c.trades[idx].Timestamp.After(cutoff) {
			break
		}
		c.currentCVD -= c.trades[idx].Volume
	}
	
	if idx > 0 {
		c.trades = c.trades[idx:]
	}

	return c.currentCVD
}
