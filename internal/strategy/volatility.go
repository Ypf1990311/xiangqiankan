package strategy

import (
	"math"
	"sync"
)

// VolatilityTracker 使用指数加权移动平均 (EWMA) 实时估计价格波动率 σ
// 这是 Avellaneda-Stoikov 最优做市模型的关键输入参数
// σ 大 → spread 应该宽（补偿高波动风险）
// σ 小 → spread 应该窄（争取更多成交机会）
type VolatilityTracker struct {
	mu         sync.RWMutex
	lastMid    float64 // 上一次的中间价
	ewmaVar    float64 // 指数加权方差 (价格空间)
	alpha      float64 // 衰减因子：越小记忆越长 (0.01 ≈ 100 tick 记忆窗口)
	warmup     int     // 已收集的样本量
	minSamples int     // 预热所需最低样本数
}

// NewVolatilityTracker 创建波动率估计器
// alpha: EWMA 衰减系数，推荐 0.005~0.02
//   - 0.005 → 约 200 tick 记忆 (平滑但迟钝)
//   - 0.01  → 约 100 tick 记忆 (平衡)
//   - 0.02  → 约 50 tick 记忆 (灵敏但嘈杂)
func NewVolatilityTracker(alpha float64) *VolatilityTracker {
	return &VolatilityTracker{
		alpha:      alpha,
		minSamples: 20, // 至少 20 个数据点才输出可靠值
	}
}

// Update 输入最新的中间价，返回当前估计的价格波动率 σ (price-space)
// 每次 evaluateAndQuote 调用时触发一次，频率约 200ms/tick
func (v *VolatilityTracker) Update(mid float64) float64 {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.lastMid == 0 || mid <= 0 {
		v.lastMid = mid
		return 0
	}

	// 价格空间变动量 (直接使用价差，不取对数，更直观且避免零值问题)
	diff := mid - v.lastMid
	v.ewmaVar = v.alpha*diff*diff + (1.0-v.alpha)*v.ewmaVar
	v.lastMid = mid
	v.warmup++

	return math.Sqrt(v.ewmaVar)
}

// GetSigma 获取当前波动率 σ 快照 (线程安全)
func (v *VolatilityTracker) GetSigma() float64 {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.warmup < v.minSamples {
		return 0 // 预热期返回 0，由调用方设置最低下限
	}
	return math.Sqrt(v.ewmaVar)
}

// IsWarmedUp 是否已完成预热
func (v *VolatilityTracker) IsWarmedUp() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.warmup >= v.minSamples
}
