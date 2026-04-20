package strategy

import (
	"alphahft/internal/orderbook"
)

type Signals struct {
	MidPrice   float64 // 传统中间价
	MicroPrice float64 // 订单簿微观加权价格
	OBI        float64 // 订单簿不平衡度 (Order Book Imbalance)
	Spread     float64 // 当前盘口买卖价差
}

// CalculateSignals 根据截面订单簿的前 N 档深度计算高频微观指标
func CalculateSignals(bids, asks []orderbook.Level, depth int) Signals {
	if len(bids) == 0 || len(asks) == 0 {
		return Signals{}
	}

	bestBid := bids[0].Price
	bestAsk := asks[0].Price
	mid := (bestBid + bestAsk) / 2.0
	spread := bestAsk - bestBid

	var bidVol, askVol float64
	limit := depth
	if len(bids) < limit {
		limit = len(bids)
	}
	if len(asks) < limit {
		limit = len(asks)
	}

	for i := 0; i < limit; i++ {
		bidVol += bids[i].Qty
		askVol += asks[i].Qty
	}

	totalVol := bidVol + askVol
	var microPrice, obi float64

	if totalVol > 0 {
		// 微观价格加权：买盘越重说明下盘支撑越强，真实的价值重心应该偏向价格更高的卖一档
		// 我们采用的公式：MicroPrice = (Ask_P * Bid_V + Bid_P * Ask_V) / (Bid_V + Ask_V)
		microPrice = (bestAsk*bidVol + bestBid*askVol) / totalVol
		
		// OBI 取值范围 [-1, 1]。 +1表示全买盘，-1表示全卖盘。
		obi = (bidVol - askVol) / totalVol
	} else {
		microPrice = mid
		obi = 0
	}

	return Signals{
		MidPrice:   mid,
		MicroPrice: microPrice,
		OBI:        obi,
		Spread:     spread,
	}
}
