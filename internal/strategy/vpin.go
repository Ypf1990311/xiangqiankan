package strategy

import (
	"math"
	"sync"
)

// VPINBucket 单个成交量桶的多空分布
type VPINBucket struct {
	BuyVol  float64
	SellVol float64
}

// VPINTracker (Volume-Synchronized Probability of Informed Trading)
// 行业标准实现：按固定成交量分桶 + 滑动窗口
//
// 与旧实现的区别：
//   - 旧: 仅用两个累加器 + 粗暴时间衰减 → 信号极不稳定
//   - 新: 按真实成交量分桶 → 统计稳健，能可靠预警知情交易者单向扫盘
type VPINTracker struct {
	mu          sync.RWMutex
	bucketSize  float64      // 每桶目标成交量 (如 5.0 BNB)
	windowSize  int          // 滑动窗口桶数 (如 20)
	buckets     []VPINBucket // 环形缓冲区
	headIndex   int          // 当前写入位置
	filledCount int          // 已填充桶数

	// 当前正在累积的桶
	currentBuy  float64
	currentSell float64
	currentVol  float64
}

// NewVPINTracker 创建标准 VPIN 追踪器
// bucketSize: 每个桶的成交量阈值 (建议设为标的 1 分钟平均成交量的 30%~50%)
// windowSize: 滑动窗口包含的桶数 (越大信号越平滑但越迟钝)
func NewVPINTracker(bucketSize float64, windowSize int) *VPINTracker {
	return &VPINTracker{
		bucketSize: bucketSize,
		windowSize: windowSize,
		buckets:    make([]VPINBucket, windowSize),
	}
}

// AddTrade 根据逐笔成交流进行成交量分桶
// isBuyerMaker: true → Taker 主动砸盘卖出 (卖量)
//
//	false → Taker 主动拉盘买入 (买量)
func (v *VPINTracker) AddTrade(isBuyerMaker bool, qty float64) {
	v.mu.Lock()
	defer v.mu.Unlock()

	// 按 Taker 方向分类
	if isBuyerMaker {
		v.currentSell += qty
	} else {
		v.currentBuy += qty
	}
	v.currentVol += qty

	// 桶满 → 推入滑动窗口
	for v.currentVol >= v.bucketSize {
		// 按比例将溢出量分配到下一个桶
		ratio := v.bucketSize / v.currentVol
		bucketBuy := v.currentBuy * ratio
		bucketSell := v.currentSell * ratio

		// 写入环形缓冲区
		v.buckets[v.headIndex] = VPINBucket{
			BuyVol:  bucketBuy,
			SellVol: bucketSell,
		}
		v.headIndex = (v.headIndex + 1) % v.windowSize
		if v.filledCount < v.windowSize {
			v.filledCount++
		}

		// 溢出量成为下一个桶的起始
		overflow := v.currentVol - v.bucketSize
		overflowBuy := v.currentBuy - bucketBuy
		overflowSell := v.currentSell - bucketSell
		v.currentBuy = overflowBuy
		v.currentSell = overflowSell
		v.currentVol = overflow
	}
}

// GetToxicity 返回 [0.0, 1.0] 的毒性指数
// 0.0 → 多空平衡，正常做市环境
// 0.5 → 中等单向压力
// 1.0 → 极端单向扫盘，知情交易者信号
func (v *VPINTracker) GetToxicity() float64 {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.filledCount == 0 {
		return 0
	}

	totalImbalance := 0.0
	totalVolume := 0.0

	count := v.filledCount
	for i := 0; i < count; i++ {
		b := v.buckets[i]
		totalImbalance += math.Abs(b.BuyVol - b.SellVol)
		totalVolume += b.BuyVol + b.SellVol
	}

	if totalVolume == 0 {
		return 0
	}

	return totalImbalance / totalVolume
}

// IsReady 判断是否有足够数据产生可靠信号 (至少填满半个窗口)
func (v *VPINTracker) IsReady() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.filledCount >= v.windowSize/2
}
