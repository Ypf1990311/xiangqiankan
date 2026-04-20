package database

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"github.com/rs/zerolog/log"
)

type TickData struct {
	Timestamp  int64
	Symbol     string
	MicroPrice float64
	Spread     float64
	OBI        float64
	CVD        float64
	VPIN       float64
}

type SignalData struct {
	Timestamp int64
	Symbol    string
	Action    string
	Price     float64
	Qty       float64
	Reason    string
}

type TelemetryDB struct {
	DB         *sql.DB
	TickChan   chan TickData
	SignalChan chan SignalData
	stopC      chan struct{}
	wg         sync.WaitGroup
}

// NewTelemetryDB 创建或打开高频度量与信号专属隔离数据库
func NewTelemetryDB(dbPath string) (*TelemetryDB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	// 初始化表
	query := `
	CREATE TABLE IF NOT EXISTS market_ticks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp INTEGER,
		symbol TEXT,
		micro_price REAL,
		spread REAL,
		obi REAL,
		cvd REAL,
		vpin REAL
	);
	CREATE INDEX IF NOT EXISTS idx_ticks_ts ON market_ticks(timestamp);

	CREATE TABLE IF NOT EXISTS action_signals (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp INTEGER,
		symbol TEXT,
		action TEXT,
		target_price REAL,
		qty REAL,
		reason TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_sig_ts ON action_signals(timestamp);
	`
	if _, err := db.Exec(query); err != nil {
		return nil, fmt.Errorf("创建 telemetry 表失败: %v", err)
	}

	// 开启最高性能的 WAL 异步写盘模式
	_, _ = db.Exec("PRAGMA journal_mode=WAL;")
	_, _ = db.Exec("PRAGMA synchronous=NORMAL;") // 允许极小概率的机器断电丢包以换取顶尖IO性能

	t := &TelemetryDB{
		DB:         db,
		TickChan:   make(chan TickData, 5000),      // 5000条缓冲，绝不卡死主线程
		SignalChan: make(chan SignalData, 1000),
		stopC:      make(chan struct{}),
	}
	
	t.StartWorker()
	return t, nil
}

// PushTick 无锁非阻塞放入行情刻度快照
func (t *TelemetryDB) PushTick(tick TickData) {
	select {
	case t.TickChan <- tick:
	default: // 如果缓冲满了直接丢弃，防止主引擎阻塞雪崩
	}
}

// PushSignal 无锁非阻塞放入交易动作信号
func (t *TelemetryDB) PushSignal(signal SignalData) {
	select {
	case t.SignalChan <- signal:
	default:
	}
}

// ClearAll 用于用户手动在页面上请求清空巨量垃圾数据
func (t *TelemetryDB) ClearAll() error {
	_, err := t.DB.Exec("DELETE FROM market_ticks; DELETE FROM action_signals; VACUUM;")
	return err
}

type ChartMarker struct {
	Time     int64  `json:"time"`
	Position string `json:"position"`
	Color    string `json:"color"`
	Shape    string `json:"shape"`
	Text     string `json:"text"`
}

// GetChartMarkers 获取经过 60 秒级别去重过滤后的 K 线标注点
func (t *TelemetryDB) GetChartMarkers(symbol string) ([]ChartMarker, error) {
	rows, err := t.DB.Query(`SELECT timestamp, action, reason FROM action_signals WHERE symbol = ? ORDER BY timestamp ASC`, symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var markers []ChartMarker
	
	// 用于按分钟聚合，防止同一分钟内出现密密麻麻相同的箭头
	lastMinuteActionMap := make(map[string]int64) 

	for rows.Next() {
		var ts int64
		var action, reason string
		_ = rows.Scan(&ts, &action, &reason)

		minuteKey := fmt.Sprintf("%s_%d", action, ts/60000)
		if _, exists := lastMinuteActionMap[minuteKey]; exists {
			continue // 这一分钟内已经画过这个颜色的箭头了，跳过
		}
		lastMinuteActionMap[minuteKey] = ts

		marker := ChartMarker{
			Time: ts / 1000, // lightweight-charts 接受的秒级时间戳
		}

		if action == "BLOCK_SELL_CVD" || action == "STRONG_BUY_SIGNAL" {
			marker.Position = "belowBar"
			marker.Color = "#10b981" // 绿色
			marker.Shape = "arrowUp"
			marker.Text = "买盘暴走"
			markers = append(markers, marker)
		} else if action == "BLOCK_BUY_CVD" || action == "STRONG_SELL_SIGNAL" {
			marker.Position = "aboveBar"
			marker.Color = "#ef4444" // 红色
			marker.Shape = "arrowDown"
			marker.Text = "雪崩抛售"
			markers = append(markers, marker)
		} else if action == "BLOCK_BUY_DIVERGENCE" {
			marker.Position = "inBar"
			marker.Color = "#eab308" // 黄色
			marker.Shape = "circle"
			marker.Text = "冰山背离"
			markers = append(markers, marker)
		}
	}
	
	return markers, nil
}

// StartWorker 后台独立协程收集并批量落盘
func (t *TelemetryDB) StartWorker() {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		
		tickBatch := make([]TickData, 0, 100)
		sigBatch := make([]SignalData, 0, 100)
		ticker := time.NewTicker(1 * time.Second) // 最慢1秒一次提交防碎片化

		flush := func() {
			if len(tickBatch) > 0 || len(sigBatch) > 0 {
				tx, err := t.DB.Begin()
				if err != nil {
					return
				}
				
				if len(tickBatch) > 0 {
					stmt, _ := tx.Prepare("INSERT INTO market_ticks (timestamp, symbol, micro_price, spread, obi, cvd, vpin) VALUES (?, ?, ?, ?, ?, ?, ?)")
					if stmt != nil {
						for _, b := range tickBatch {
							_, _ = stmt.Exec(b.Timestamp, b.Symbol, b.MicroPrice, b.Spread, b.OBI, b.CVD, b.VPIN)
						}
						stmt.Close()
					}
					tickBatch = tickBatch[:0]
				}

				if len(sigBatch) > 0 {
					stmt, _ := tx.Prepare("INSERT INTO action_signals (timestamp, symbol, action, target_price, qty, reason) VALUES (?, ?, ?, ?, ?, ?)")
					if stmt != nil {
						for _, s := range sigBatch {
							_, _ = stmt.Exec(s.Timestamp, s.Symbol, s.Action, s.Price, s.Qty, s.Reason)
						}
						stmt.Close()
					}
					sigBatch = sigBatch[:0]
				}
				
				_ = tx.Commit()
			}
		}

		for {
			select {
			case <-t.stopC:
				flush() // 关机前最后刷一次
				return
			case tick := <-t.TickChan:
				tickBatch = append(tickBatch, tick)
				if len(tickBatch) >= 100 {
					flush()
				}
			case sig := <-t.SignalChan:
				sigBatch = append(sigBatch, sig)
				if len(sigBatch) >= 100 {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()
}

// Stop 停止工作协程
func (t *TelemetryDB) Stop() {
	close(t.stopC)
	t.wg.Wait()
	t.DB.Close()
	log.Info().Msg("🛑 遥测异步存储仓已安全关闭")
}
