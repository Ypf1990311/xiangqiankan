package monitor

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

var (
	// PromRealizedPnL 存储每日累计获得的真金白银利润 (Testnet / PaperTrading)
	PromRealizedPnL = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hft_realized_pnl",
		Help: "累计已实现赚钱盈亏",
	}, []string{"symbol"})

	// PromUnrealizedPnL 存储目前的悬浮头寸浮盈
	PromUnrealizedPnL = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hft_unrealized_pnl",
		Help: "动态持仓浮动盈亏",
	}, []string{"symbol"})

	// PromPosition 存储当前的头寸规模 (多或空)
	PromPosition = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hft_position_size",
		Help: "当前持仓数量(带正负号)",
	}, []string{"symbol"})

	// PromMicroPrice 我们引以为傲的微观价格体系曲线
	PromMicroPrice = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hft_micro_price",
		Help: "实时探测量子微观价格(非中间价)",
	}, []string{"symbol"})

	// PromOBI OBI失衡打分曲线图
	PromOBI = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hft_orderbook_imbalance",
		Help: "盘口买卖失衡指标 (OBI)",
	}, []string{"symbol"})

	// PromVPIN VPIN毒性指标，用于观察被碾压的高危时刻
	PromVPIN = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hft_vpin_toxicity",
		Help: "单边毒性流量指标(防止做单被淹没)",
	}, []string{"symbol"})

	// PromCVD 主动买卖累计差，判定量价背离的关键
	PromCVD = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hft_cvd_delta",
		Help: "Cumulative Volume Delta 主动买卖累计吃单差额",
	}, []string{"symbol"})
)

// StartPrometheusServer 在后台自动起一个网页 API，把以上指标按照 Prometheus 标准导出
func StartPrometheusServer(port string) {
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Info().Msgf("📊 [Grafana 指标雷达] 看板数据流开启，正随时往外投射光斑信号: http://localhost:%s/metrics", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Error().Err(err).Msg("❌ Prometheus 服务通道启动失败，无法向外部 Grafana 供电")
		}
	}()
}
