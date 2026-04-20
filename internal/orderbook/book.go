package orderbook

import (
	"sync"
)

// Level 代表订单簿上的一个价格档位
type Level struct {
	Price float64
	Qty   float64
}

// OrderBook 维护单个交易对的本地 Level 2 切片
// 为了追求极端速度和避免 Map/Tree 的 GC 压力，我们直接使用切片，
// 并通过读写锁 (RWMutex) 保护。由于高频主要关注前几档，切片迭代速度比任何树都快。
type OrderBook struct {
	Symbol     string
	Bids       []Level // 买盘 (降序排列，即第一位是最高买价)
	Asks       []Level // 卖盘 (升序排列，即第一位是最低卖价)
	UpdateTime int64   // 数据更新时间 (毫秒级时间戳)
	OnUpdate   func()  // 🚀 [V37.0] 更新回调钩子
	mu         sync.RWMutex
}

// NewOrderBook 初始化一个新的记录器
func NewOrderBook(symbol string) *OrderBook {
	return &OrderBook{
		Symbol: symbol,
		// 预先分配容量（比如20档）防内存重新分配
		Bids: make([]Level, 0, 50),
		Asks: make([]Level, 0, 50),
	}
}

// UpdateSnapshot 使用交易所推送的最新快照完全覆盖订单簿
// bids 和 asks 必须已经提前排序好
func (ob *OrderBook) UpdateSnapshot(bids, asks []Level, updateTime int64) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	ob.Bids = bids
	ob.Asks = asks
	if updateTime > ob.UpdateTime {
		ob.UpdateTime = updateTime
	}

	// 🛰️ [V37.0] 行情更新后，如果绑定了回调，则立刻通知触发
	if ob.OnUpdate != nil {
		ob.OnUpdate()
	}
}

// GetTop 获取当前买一和卖一的聚合信息 (极低延迟无分配查询)
func (ob *OrderBook) GetTop() (bestBid, bestAsk Level, ok bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	if len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return Level{}, Level{}, false
	}
	return ob.Bids[0], ob.Asks[0], true
}

// CopySnapshot 创建当前订单簿的完整拷贝，传给策略引擎用于复杂计算(如截面微观价格、OBI)
func (ob *OrderBook) CopySnapshot() (bids, asks []Level, ts int64) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	bidsCopy := make([]Level, len(ob.Bids))
	copy(bidsCopy, ob.Bids)

	asksCopy := make([]Level, len(ob.Asks))
	copy(asksCopy, ob.Asks)

	return bidsCopy, asksCopy, ob.UpdateTime
}
