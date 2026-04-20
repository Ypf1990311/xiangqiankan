package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"alphahft/config"
	binance_exchange "alphahft/internal/exchange/binance"
	"alphahft/internal/database"
	"alphahft/internal/monitor"
	"alphahft/internal/notify"
	"alphahft/internal/orderbook"
	"alphahft/internal/risk"
	"alphahft/internal/simulator"
	"alphahft/internal/strategy"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/rs/zerolog/log"
)

func main() {
	// 1. 初始化日志（纳秒精度，支持 HFT 调试）
	monitor.InitLogger()
	log.Info().Msg("🚀 启动 AlphaHFT-Perp 高频做市引擎...")

	// 2. 加载配置文件 (运行时一般以项目根目录为 CWD)
	cfgPath := "config/config.yaml"
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Warn().Err(err).Str("path", cfgPath).Msg("⚠️ 无法加载配置文件，将使用默认配置(Testnet)")
		cfg = &config.Config{
			Exchange: config.ExchangeConfig{
				Testnet: true,
			},
		}
	}

	log.Info().Bool("Testnet", cfg.Exchange.Testnet).Msg("📡 准备连接交易所")

	// 3. 初始化币安合约 REST API
	client := binance_exchange.NewFuturesClient(
		cfg.Exchange.APIKey,
		cfg.Exchange.Secret,
		cfg.Exchange.Testnet,
	)

	// 4. 初始化引擎前置动作：如果填了有效的 API Key，尝试开启对冲模式
	if cfg.Exchange.APIKey != "" && cfg.Exchange.APIKey != "YOUR_API_KEY" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = client.DisableHedgeMode(ctx)

		// 将杠杆倍数调整为用户要求的 10x
		for _, sym := range cfg.Symbols {
			_ = client.ChangeLeverage(ctx, sym, 10) 
		}

	} else {
		log.Warn().Msg("未检测到有效的 API Key，跳过执行账户维度的初始设置 (如重置杠杆等)。")
	}

	// 5. 初始化电报告警机器人系统并发送破冰消息
	tg := notify.NewTelegramNotifier(cfg.Notify.TelegramToken, cfg.Notify.TelegramChatID)
	tg.Send("✅ <b>高频交易引擎已上线</b>\n实时探测策略：MicroPrice & OBI \n目前护城河: 5x | 单向持仓\n<i>正在发足马力提取微小波动中...</i>")

	monitor.StartPrometheusServer("2112")

	// ⭐️ [V17.5] 初始化持久化数据库
	ledger, err := database.NewLedger("alphahft.db")
	if err != nil {
		log.Fatal().Err(err).Msg("❌ 无法启动 SQLite 数据库")
	}
	// 🧹 [V46.0] 洁癖启动逻辑：根据用户要求，启动即彻底清空历史持仓记录
	_ = ledger.FlushActiveChunks()
	log.Info().Msg("🧹 [系统启动] 已成功清空数据库历史成交梯度，将从空仓开始重新记账")

	// ⭐️ [New] 初始化高频遥测信号接收库
	telemetry, err := database.NewTelemetryDB("signals_metrics.db")
	if err != nil {
		log.Fatal().Err(err).Msg("❌ 无法启动 Telemetry 遥测数据库")
	}

	// 6. ✨ 驱动层：利用 goroutine 并发开启配置图纸上所有的币种列阵
	var makerEngines []*strategy.MakerEngine
	var marketStreams []*binance_exchange.MarketStream

	// 📡 [跨品种监控] 实例化唯一的 BTC 全网风控哨兵
	globalLeadLag := strategy.NewLeadLagTracker("BTCUSDT", -30.0, 30.0) // 1秒激跌跌、暴涨超 30 刀报警
	globalLeadLag.Start(context.Background())

	for _, sym := range cfg.Symbols {
		if sym == "" {
			continue
		}
		
		ob := orderbook.NewOrderBook(sym)
		
		// 行情流
		mStream := binance_exchange.NewMarketStream(sym, ob)
		mStream.Start()
		marketStreams = append(marketStreams, mStream)

		// 创建当前品种的模拟沙盘
		pEngine := simulator.NewPaperEngine(sym, 5000.0)

		// 毒性探测器与 CVD 累计动能追踪器
		vpin := strategy.NewVPINTracker(5.0, 20) // 5 BNB/桶, 20 桶滑动窗口
		cvdTracker := strategy.NewCVDTracker(30 * time.Second) // 30秒 CVD 窗口

		_, _, _ = futures.WsAggTradeServe(sym, func(event *futures.WsAggTradeEvent) {
			qty, _ := strconv.ParseFloat(event.Quantity, 64)
			price, _ := strconv.ParseFloat(event.Price, 64)
			
			// 1. 投喂 VPIN 与 CVD
			vpin.AddTrade(event.Maker, qty)
			cvdTracker.AddTrade(event.Maker, qty)

			// 将 CVD 指标灌入 Prometheus 画图
			monitor.PromCVD.WithLabelValues(sym).Set(cvdTracker.GetCVD())
			
			// 2. 如果开启沙盘，将真实成交实时喂给模拟盘进行碰撞
			if cfg.Exchange.PaperTrade {
				pEngine.CheckFill(price)
			}
			
		}, func(err error) {
			log.Warn().Err(err).Str("sym", sym).Msg("AggTrade 逐笔流中断")
		})

		// 风险引擎
		rEngine := risk.NewRiskEngine(client, sym)
		rEngine.Notifier = tg
		rEngine.MaxDailyLoss = 50.0 // 每日最大允许亏损 $50
		rEngine.StartRoutine()

		// 启动私有流 (用于精准捕捉成交记录)
		uStream := binance_exchange.NewUserStream(client)
		_ = uStream.Start(context.Background())

		// 决策引擎主体
		mEngine := strategy.NewMakerEngine(sym, ob, client, cfg, rEngine, ledger)
		mEngine.VPIN = vpin // 安装毒性监控
		mEngine.CVD = cvdTracker // 安装 CVD 探测核心
		mEngine.LeadLag = globalLeadLag // 安装跨所领跑哨兵
		mEngine.Paper = pEngine // 安装模拟盘
		mEngine.Telemetry = telemetry // 安装新版遥测黑匣子
		
		// ⭐️ 根据用户最新要求：500USDC 持仓上限
		if sym == "BNBUSDC" {
			mEngine.OrderQty = 0.02          // 单笔约 12~13 U
			mEngine.MaxPosAllowed = 0.81     // 约 510~515 U
			mEngine.InventoryGamma = 0.5     // A-S 库存偏斜敏感度
			mEngine.SpreadFactor = 1.5       // 波动率 spread 乘数
		}

		mEngine.Start(uStream.TradeChan)
		makerEngines = append(makerEngines, mEngine)

		// 🚀 实时仓位同步：WebSocket → RiskEngine (延迟 <50ms)
		go func(us *binance_exchange.UserStream, re *risk.RiskEngine, symbol string) {
			for update := range us.PositionChan {
				if update.Symbol == symbol {
					re.UpdatePosition(update.PosAmt, update.EntryPrice, update.Unrealized)
				}
			}
		}(uStream, rEngine, sym)
	}

	// ⭐️⭐️ [新增] 基于 Go 内嵌 Server 的极其简单的 API 接口
	http.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		
		type SymData struct {
			Symbol      string  `json:"symbol"`
			Realized    float64 `json:"realized"`
			Unrealized  float64 `json:"unrealized"`
			Pos         float64 `json:"position"`
			Toxicity    float64 `json:"toxicity"`
			Session     float64 `json:"session_pnl"`
			Balance     float64 `json:"total_balance"` // 👈 [新增] 实时的钱包余额
			InvGamma    float64 `json:"inv_gamma"`
			MaxPos      float64 `json:"max_pos"`
			Spread      float64 `json:"spread"`
			Qty         float64 `json:"qty"`
		}
		
		// 获取最新账户信息
		acc, _ := client.GetAccount(context.Background())
		var totalBalance float64
		if acc != nil {
			for _, a := range acc.Assets {
				if a.Asset == "USDC" || a.Asset == "USDT" {
					val, _ := strconv.ParseFloat(a.WalletBalance, 64)
					totalBalance += val
				}
			}
		}

		results := []SymData{}
		for _, me := range makerEngines {
			d := SymData{
				Symbol: me.Symbol,
				Balance: totalBalance, // 每个币种都带上总余额
				InvGamma: me.InventoryGamma,
				MaxPos: me.MaxPosAllowed,
				Spread: me.SpreadFactor,
				Qty: me.OrderQty,
			}
			if me.Config.Exchange.PaperTrade && me.Paper != nil {
				d.Realized = me.Paper.RealizedPnL
				d.Unrealized = me.Paper.UnrealizedPnL
				d.Pos = me.Paper.PositionAmt
				d.Session = me.Paper.RealizedPnL // 模拟盘 Session 等于已实现
			} else if me.RiskEngine != nil {
				d.Realized = me.RiskEngine.SessionProfit // 实盘 Session
				d.Unrealized = me.RiskEngine.Unrealized
				d.Pos = me.RiskEngine.PosAmt
				d.Session = me.RiskEngine.SessionProfit
			}
			if me.VPIN != nil {
				d.Toxicity = me.VPIN.GetToxicity()
			}
			results = append(results, d)
		}

		json.NewEncoder(w).Encode(results)
	})

	// ⭐️ [V40.0] 阵地活动订单视图 (改版：聚合展示弹夹库存)
	http.HandleFunc("/api/magazine", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		results, err := ledger.GetAggregatedChunks(makerEngines[0].Symbol)
		if err != nil {
			log.Error().Err(err).Msg("Failed to query aggregated magazine")
			return
		}
		json.NewEncoder(w).Encode(results)
	})

	// ⭐️ [新增] 参数动态热修改接口
	http.HandleFunc("/api/tune", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions { return }
		if r.Method != http.MethodPost { return }

		var update struct {
			Gamma    float64 `json:"gamma"`
			Max      float64 `json:"max"`
			Qty      float64 `json:"qty"`
			Spr      float64 `json:"spr"`
			CvdFlat  float64 `json:"cvd_flat"` // 空仓 CVD 鐠值
			CvdPos   float64 `json:"cvd_pos"`  // 持仓 CVD 鐠值
			Obi      float64 `json:"obi"`      // OBI 鐠值
		}
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil { return }

		for _, me := range makerEngines {
			if update.Gamma > 0 { me.InventoryGamma = update.Gamma }
			if update.Max > 0 { me.MaxPosAllowed = update.Max }
			if update.Qty > 0 { me.OrderQty = update.Qty }
			if update.Spr > 0 { me.SpreadFactor = update.Spr }
			if update.CvdFlat > 0 { me.CVDFlatThreshold = update.CvdFlat }
			if update.CvdPos > 0 { me.CVDPosThreshold = update.CvdPos }
			if update.Obi > 0 { me.OBIThreshold = update.Obi }
		}
		log.Warn().Msgf("🎯 [热同步] 调参指令下达: Gamma=%.2f Max=%.2f Qty=%.3f Spr=%.2f CVDFlat=%.1f CVDPos=%.1f OBI=%.2f",
			update.Gamma, update.Max, update.Qty, update.Spr, update.CvdFlat, update.CvdPos, update.Obi)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// ⭐️ [新增] 引擎实时状态回显接口 (供面板读取当前参数)
	http.HandleFunc("/api/engine_state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		type EngineState struct {
			Symbol          string  `json:"symbol"`
			Gamma           float64 `json:"gamma"`
			SpreadFactor    float64 `json:"spread_factor"`
			OrderQty        float64 `json:"order_qty"`
			MaxPos          float64 `json:"max_pos"`
			CVDFlatThreshold float64 `json:"cvd_flat"`
			CVDPosThreshold  float64 `json:"cvd_pos"`
			OBIThreshold     float64 `json:"obi"`
			CVDNow          float64 `json:"cvd_now"`
			PosAmt          float64 `json:"pos_amt"`
			AvgEntry        float64 `json:"avg_entry"`
		}
		var states []EngineState
		for _, me := range makerEngines {
			cvdNow := 0.0
			if me.CVD != nil { cvdNow = me.CVD.GetCVD() }
			states = append(states, EngineState{
				Symbol:          me.Symbol,
				Gamma:           me.InventoryGamma,
				SpreadFactor:    me.SpreadFactor,
				OrderQty:        me.OrderQty,
				MaxPos:          me.MaxPosAllowed,
				CVDFlatThreshold: me.CVDFlatThreshold,
				CVDPosThreshold:  me.CVDPosThreshold,
				OBIThreshold:     me.OBIThreshold,
				CVDNow:          cvdNow,
				PosAmt:          me.RiskEngine.PosAmt,
				AvgEntry:        me.RiskEngine.EntryPrice,
			})
		}
		json.NewEncoder(w).Encode(states)
	})

	// ⭐️ [新增] 手动清理高频遥测库数据的接口
	http.HandleFunc("/api/clear_db", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method != http.MethodPost { return }
		err := telemetry.ClearAll()
		if err != nil {
			w.Write([]byte(`{"status":"error", "msg":"` + err.Error() + `"}`))
			return
		}
		w.Write([]byte(`{"status":"ok", "msg":"数据已清空"}`))
	})

	// ⭐️ [新增] 获取可视化 K 线雷达信号接口
	http.HandleFunc("/api/chart_signals", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		// 暂且硬编码，如果是多币种可以通过URL传参获取
		markers, err := telemetry.GetChartMarkers("BNBUSDC")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if markers == nil {
			markers = []database.ChartMarker{}
		}
		b, _ := json.Marshal(markers)
		w.Write(b)
	})

	// 定时权益快照（每10分钟一次）
	go func() {
		for {
			acc, err := client.GetAccount(context.Background())
			if err == nil {
				total := 0.0
				for _, a := range acc.Assets {
					// 兼容 USDC 和 USDT 结算资产
					if strings.Contains(a.Asset, "USD") {
						total, _ = strconv.ParseFloat(a.WalletBalance, 64)
						break
					}
				}
				if total > 0 {
					ledger.RecordEquity(total)
					log.Info().Float64("total_equity", total).Msg("💾 资产快照已存入数据库")
				}
			}
			time.Sleep(10 * time.Minute)
		}
	}()

	// ⭐️ [V7.0] 资产历史数据接口
	http.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		data, _ := ledger.GetEquityCurve()
		json.NewEncoder(w).Encode(data)
	})


	// ⭐️ [新增] 简易文字大盘播报协程
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		for range ticker.C {
			if len(makerEngines) == 0 { continue }
			
			fmt.Println("\n" + strings.Repeat("=", 64))
			fmt.Printf("🚀 ALPHA-HFT 驾驶舱 | 状态: 正常运行 | 时间: %s\n", time.Now().Format("15:04:05"))
			fmt.Println(strings.Repeat("-", 64))
			
			var totalSession float64
			modeName := "LIVE"
			if makerEngines[0].Config.Exchange.PaperTrade {
				modeName = "PAPER"
			}

			for _, me := range makerEngines {
				var pnl, urpnl, pos, entry float64
				if me.Config.Exchange.PaperTrade && me.Paper != nil {
					pnl = me.Paper.RealizedPnL
					urpnl = me.Paper.UnrealizedPnL
					pos = me.Paper.PositionAmt
					entry = me.Paper.AvgEntryPrice
				} else {
					_, pnl = me.RiskEngine.GetSessionStats()
					pos, entry, urpnl, _ = me.RiskEngine.GetPositionSnapshot()
				}
				totalSession += pnl
				
				// 提取市场参数
				obBids, obAsks, _ := me.OB.CopySnapshot()
				obi := 0.0
				microPrice := 0.0
				if len(obBids) > 0 && len(obAsks) > 0 {
					sig := strategy.CalculateSignals(obBids, obAsks, 5)
					obi = sig.OBI
					microPrice = sig.MicroPrice
				}
				
				vpinStr := "0.00"
				if me.VPIN != nil {
					vpinStr = fmt.Sprintf("%.2f", me.VPIN.GetToxicity())
				}
				cvdStr := "0.0"
				if me.CVD != nil {
					cvdStr = fmt.Sprintf("%.1f", me.CVD.GetCVD())
				}
				btcSafe := "✅"
				if me.LeadLag != nil {
					if me.LeadLag.IsBearDanger() {
						btcSafe = "🚨跌!"
					} else if me.LeadLag.IsBullDanger() {
						btcSafe = "🚀涨!"
					}
				}

				posValue := math.Abs(pos * microPrice)
				fmt.Printf("🪙 %-8s: 收益: %+7.3f U | 浮盈: %+7.3f U | 仓位: %+7.4f (≈%.1fU) | 成本: %.2f\n", 
					me.Symbol, pnl, urpnl, pos, posValue, entry)
				fmt.Printf("📊 行情雷达: OBI: %+5.2f | CVD: %-6s | 毒性: %-4s | 现价: %.2f | 大盘防线: %s\n", 
					obi, cvdStr, vpinStr, microPrice, btcSafe)
				fmt.Printf("⚔️ 高频交锋: 配对: %d 笔 | 纯利润: %+7.4f U\n", 
					me.RoundTrips, me.RoundTripProfit)
			}
			
			fmt.Println(strings.Repeat("-", 64))
			fmt.Printf("💰 本次运行总计收益: %.3f U (%s Mode)\n", totalSession, modeName)
			fmt.Println(strings.Repeat("=", 64) + "\n")

		}
	}()

	// ⭐️ [新增] 直接把网页喂给浏览器，URL: http://localhost:2112/dashboard
	http.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/dashboard.html")
	})

	http.HandleFunc("/api/fills", func(w http.ResponseWriter, r *http.Request) {
		fills, _ := ledger.GetFills(50)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fills)
	})

	log.Info().Msg("✅ 引擎核心车间矩阵点火装载完毕。")
	log.Info().Msg("📊 可视化仪表盘已上线：http://localhost:2112/dashboard")
	log.Info().Msg("等待长官下达中断信号 (Ctrl+C)...")



	// 8. 捕获中断信号优雅停机
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Warn().Msg("🛑 正在连线撤销云端所有防线的活动挂单...")
	
	for _, engine := range makerEngines {
		engine.Stop()
	}
	for _, stream := range marketStreams {
		stream.Stop()
	}
	
	telemetry.Stop() // 关闭遥测存储进程
	
	// 这里通过 REST API 最快速度地撤销该交易对上所有的挂单，避免程序死掉却留有买盘
	if cfg.Exchange.APIKey != "" && cfg.Exchange.APIKey != "YOUR_API_KEY" {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, sym := range cfg.Symbols {
			_ = client.CancelAllOpenOrders(shutdownCtx, sym)
		}
	}

	log.Info().Msg("🛑 安全清理完成，兵团顺利退场，再见！")
}
