package binance

import (
	"context"
	"strconv"
	"strings"

	binance "github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/rs/zerolog/log"
)

type FuturesClient struct {
	Client *futures.Client
}

func NewFuturesClient(apiKey, secret string, testnet bool) *FuturesClient {
	if testnet {
		futures.UseTestnet = true
	}
	client := binance.NewFuturesClient(apiKey, secret)
	
	err := client.NewPingService().Do(context.Background())
	if err != nil {
		log.Error().Err(err).Msg("未能连接到 Binance Futures REST API")
	} else {
		log.Info().Msg("Binance Futures REST API - Ping 成功")
	}

	return &FuturesClient{
		Client: client,
	}
}

// DisableHedgeMode 设置账户为单向持仓模式 (对于高频自动对冲更简单)
func (c *FuturesClient) DisableHedgeMode(ctx context.Context) error {
	err := c.Client.NewChangePositionModeService().DualSide(false).Do(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("切换单向持仓模式提示失败（可能已经开启过了，请忽略）")
		return err
	}
	log.Info().Msg("已成功将账户切换为 单向持仓 (One-way Mode)")
	return nil
}

// ChangeLeverage 调整杠杆级别
func (c *FuturesClient) ChangeLeverage(ctx context.Context, symbol string, leverage int) error {
	res, err := c.Client.NewChangeLeverageService().Symbol(symbol).Leverage(leverage).Do(ctx)
	if err != nil {
		log.Error().Err(err).Str("symbol", symbol).Msg("调整杠杆失败")
		return err
	}
	log.Info().Int("leverage", res.Leverage).Str("symbol", res.Symbol).Msg("杠杆倍数已更新")
	return nil
}

// ========================
// Phase 3: 发单与风控查询扩展
// ========================

// GetPositionRisk 获取实时的仓位和爆仓风险
func (c *FuturesClient) GetPositionRisk(ctx context.Context, symbol string) (*futures.PositionRisk, error) {
	risks, err := c.Client.NewGetPositionRiskService().Symbol(symbol).Do(ctx)
	if err != nil || len(risks) == 0 {
		return nil, err
	}
	return risks[0], nil
}

// GetFundingRate 获取当前标的的预估资金费率
func (c *FuturesClient) GetFundingRate(ctx context.Context, symbol string) (float64, error) {
	premium, err := c.Client.NewPremiumIndexService().Symbol(symbol).Do(ctx)
	if err != nil || len(premium) == 0 {
		return 0, err
	}
	rateStr := premium[0].LastFundingRate
	rate, _ := strconv.ParseFloat(rateStr, 64)
	return rate, nil
}

// CancelAllOpenOrders 撤销所有本币种的挂单（高频刷单的核心：先全撤，再重挂）
func (c *FuturesClient) CancelAllOpenOrders(ctx context.Context, symbol string) error {
	err := c.Client.NewCancelAllOpenOrdersService().Symbol(symbol).Do(ctx)
	if err != nil {
		log.Error().Err(err).Msg("❌ 取消订单失败")
	}
	return err
}

// CancelOrder 撤销单笔指定订单
func (c *FuturesClient) CancelOrder(ctx context.Context, symbol string, orderID int64) error {
	_, err := c.Client.NewCancelOrderService().Symbol(symbol).OrderID(orderID).Do(ctx)
	if err != nil {
		if !strings.Contains(err.Error(), "Unknown order") {
			log.Error().Err(err).Int64("orderID", orderID).Msg("❌ 撤销单笔挂单碰壁")
		}
	}
	return err
}

// PlaceMakerOrder 创建交易订单，根据 postOnly 决定是强制做市（GTX）还是普通限价（GTC）
func (c *FuturesClient) PlaceMakerOrder(ctx context.Context, symbol string, side futures.SideType, price, qty float64, pricePrecision, qtyPrecision int, postOnly bool) (int64, error) {
	priceStr := strconv.FormatFloat(price, 'f', pricePrecision, 64) 
	qtyStr := strconv.FormatFloat(qty, 'f', qtyPrecision, 64)

	tif := futures.TimeInForceTypeGTX // 默认 GTX = Post Only 绝对做市商
	if !postOnly {
		tif = futures.TimeInForceTypeGTC // GTC = 允许直接吃单成交 (Taker)
	}

	res, err := c.Client.NewCreateOrderService().
		Symbol(symbol).
		Side(side).
		Type(futures.OrderTypeLimit).
		TimeInForce(tif).
		Price(priceStr).
		Quantity(qtyStr).
		Do(ctx)
	
	if err != nil {
		// 屏蔽 -5022 (Post Only will be rejected) 造成的无关痛痒的刷屏警告
		if strings.Contains(err.Error(), "-5022") { return 0, err }
		log.Error().Err(err).Str("side", string(side)).Str("price", priceStr).Msg("❌ 挂单碰壁或失败")
		return 0, err
	}
	return res.OrderID, nil
}

// GetAccount 获取账户完整信息 (用于快照余额)
func (c *FuturesClient) GetAccount(ctx context.Context) (*futures.Account, error) {
	return c.Client.NewGetAccountService().Do(ctx)
}

// PlaceMarketOrder 创建市价订单（用于紧急减仓或止盈）
func (c *FuturesClient) PlaceMarketOrder(ctx context.Context, symbol string, side futures.SideType, qty float64, qtyPrecision int) (int64, error) {
	qtyStr := strconv.FormatFloat(qty, 'f', qtyPrecision, 64)

	res, err := c.Client.NewCreateOrderService().
		Symbol(symbol).
		Side(side).
		Type(futures.OrderTypeMarket).
		Quantity(qtyStr).
		Do(ctx)

	if err != nil {
		log.Error().Err(err).Str("side", string(side)).Str("qty", qtyStr).Msg("❌ 市价单下单失败")
		return 0, err
	}
	return res.OrderID, nil
}
