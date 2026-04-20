package database

import (
	"database/sql"
	"fmt"
	"math"
	"time"

	_ "modernc.org/sqlite" 
)

type TradeChunk struct {
	ID        int     `json:"id"`
	Price     float64 `json:"price"`
	Qty       float64 `json:"qty"`
	Remaining float64 `json:"remaining"`
}

type Ledger struct {
	DB *sql.DB
}

func NewLedger(dbPath string) (*Ledger, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	// 初始化表结构：记录流水、每日权益以及【V17.5】颗粒弹夹
	query := `
	CREATE TABLE IF NOT EXISTS income_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp INTEGER,
		income REAL,
		type TEXT,
		asset TEXT,
		symbol TEXT
	);
	CREATE TABLE IF NOT EXISTS daily_equity (
		date TEXT PRIMARY KEY,
		balance REAL
	);
	CREATE TABLE IF NOT EXISTS trade_chunks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		symbol TEXT,
		side TEXT,
		price REAL,
		qty REAL,
		remaining REAL,
		timestamp INTEGER
	);
	CREATE TABLE IF NOT EXISTS fill_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		symbol TEXT,
		side TEXT,
		price REAL,
		qty REAL,
		pnl REAL,
		timestamp INTEGER
	);
	`
	_, err = db.Exec(query)
	if err == nil {
		// 🚀 [V37.0] 开启 WAL 模式，大幅提升超高频成交时的数据库并发写入性能
		_, _ = db.Exec("PRAGMA journal_mode=WAL;")
		_, _ = db.Exec("PRAGMA synchronous=NORMAL;")
	}
	return &Ledger{DB: db}, err
}

func (l *Ledger) SaveChunk(symbol, side string, price, qty float64) error {
	_, err := l.DB.Exec(`INSERT INTO trade_chunks (symbol, side, price, qty, remaining, timestamp) 
		VALUES (?, ?, ?, ?, ?, ?)`, symbol, side, price, qty, qty, time.Now().UnixMilli())
	return err
}

func (l *Ledger) PopChunk(symbol, targetSide string, qty float64) error {
	// 价格优先原则：找到利润空间最大（价格最优）的订单
	// 多头平仓：挑入场价最低的单子 (price ASC)；空头平仓：挑入场价最高的单子 (price DESC)
	orderClause := "price ASC"
	if targetSide == "SELL" {
		orderClause = "price DESC"
	}

	remainingToPop := qty
	for remainingToPop > 0 {
		var id int
		var rem float64
		query := `SELECT id, remaining FROM trade_chunks 
			WHERE symbol = ? AND side = ? AND remaining > 0 
			ORDER BY ` + orderClause + `, id DESC LIMIT 1`
		err := l.DB.QueryRow(query, symbol, targetSide).Scan(&id, &rem)
		
		if err != nil { break } // 没货了

		if rem <= remainingToPop + 0.00001 {
			_, _ = l.DB.Exec("UPDATE trade_chunks SET remaining = 0 WHERE id = ?", id)
			remainingToPop -= rem
		} else {
			_, _ = l.DB.Exec("UPDATE trade_chunks SET remaining = remaining - ? WHERE id = ?", remainingToPop, id)
			remainingToPop = 0
		}
	}
	return nil
}

func (l *Ledger) LoadChunks(symbol, side string) ([]TradeChunk, error) {
	// 加载时按价格优先排序，确保内存栈顶（Slice 末尾）是利润最大的单子
	// 多头 (BUY)：价格从高到低排列 [643, 622, 601] -> 末尾是 601 (最低价)
	// 空头 (SELL)：价格从低到高排列 [601, 615, 624] -> 末尾是 624 (最高价)
	orderClause := "price DESC" 
	if side == "SELL" {
		orderClause = "price ASC"
	}

	query := fmt.Sprintf("SELECT id, price, remaining FROM trade_chunks WHERE symbol = ? AND side = ? AND remaining > 0 ORDER BY %s", orderClause)
	rows, err := l.DB.Query(query, symbol, side)
	if err != nil { return nil, err }
	defer rows.Close()

	var results []TradeChunk
	for rows.Next() {
		var id int
		var p, r float64
		rows.Scan(&id, &p, &r)
		results = append(results, TradeChunk{ID: id, Price: p, Qty: r})
	}
	return results, nil
}

// [新增] 锚点式强制对账：以交易所真实仓位为准，逆向寻找开仓“锚点”，清理历史僵尸单
func (l *Ledger) SyncWithExchange(symbol string, actualPos float64) error {
	// 1. 全部清写：先假设所有单子都是关闭的 (内部逻辑，不直接 Exec)
	_, _ = l.DB.Exec("UPDATE trade_chunks SET remaining = 0 WHERE symbol = ?", symbol)

	if math.Abs(actualPos) < 0.0001 { return nil }

	side := "BUY"
	if actualPos < 0 { side = "SELL" }
	targetQty := math.Abs(actualPos)

	// 2. 逆向寻找锚点：从最新的对应方向订单开始加和
	rows, err := l.DB.Query(`SELECT id, qty FROM trade_chunks 
		WHERE symbol = ? AND side = ? ORDER BY id DESC`, symbol, side)
	if err != nil { return err }
	defer rows.Close()

	var matchedIDs []int
	var lastID int
	var lastPartialQty float64
	accumulated := 0.0

	for rows.Next() {
		var id int
		var qty float64
		rows.Scan(&id, &qty)
		
		if accumulated + qty <= targetQty + 0.00001 {
			matchedIDs = append(matchedIDs, id)
			accumulated += qty
			if math.Abs(accumulated - targetQty) < 0.00001 {
				break
			}
		} else {
			// 这是锚点订单，只需要它的一部分内容
			lastID = id
			lastPartialQty = targetQty - accumulated
			accumulated = targetQty
			break
		}
	}

	// 3. 恢复幸存订单的 remaining 值
	for _, id := range matchedIDs {
		_, _ = l.DB.Exec("UPDATE trade_chunks SET remaining = qty WHERE id = ?", id)
	}
	if lastID > 0 {
		_, _ = l.DB.Exec("UPDATE trade_chunks SET remaining = ? WHERE id = ?", lastPartialQty, lastID)
	}

	return nil
}

// [新增] 聚合订单展示：按价格合并显示，减少 UI 噪音
type AggregatedChunk struct {
	Price     float64 `json:"price"`
	TotalQty  float64 `json:"qty"`
	OrderCount int     `json:"count"`
	Side      string  `json:"side"`
}

func (l *Ledger) GetAggregatedChunks(symbol string) ([]AggregatedChunk, error) {
	query := `SELECT price, SUM(remaining), COUNT(*), side FROM trade_chunks 
			  WHERE symbol = ? AND remaining > 0 
			  GROUP BY side, ROUND(price, 2) 
			  ORDER BY price DESC`
	rows, err := l.DB.Query(query, symbol)
	if err != nil { return nil, err }
	defer rows.Close()

	var results []AggregatedChunk
	for rows.Next() {
		var a AggregatedChunk
		rows.Scan(&a.Price, &a.TotalQty, &a.OrderCount, &a.Side)
		results = append(results, a)
	}
	return results, nil
}

func (l *Ledger) RecordEquity(balance float64) error {
	today := time.Now().Format("2006-01-02 15:04:05")
	_, err := l.DB.Exec("INSERT INTO daily_equity (date, balance) VALUES (?, ?)", today, balance)
	return err
}

func (l *Ledger) GetDailyEquity() ([]map[string]interface{}, error) {
	rows, err := l.DB.Query("SELECT date, balance FROM daily_equity ORDER BY date ASC")
	if err != nil { return nil, err }
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var d string
		var b float64
		rows.Scan(&d, &b)
		results = append(results, map[string]interface{}{"date": d, "balance": b})
	}
	return results, nil
}

// [V46.0] 洁癖式启动：清空所有处于 Active 状态的持仓弹夹，确保账实相符
func (l *Ledger) FlushActiveChunks() error {
	_, err := l.DB.Exec("DELETE FROM trade_chunks")
	return err
}

func (l *Ledger) GetEquityCurve() ([]map[string]interface{}, error) {
	rows, err := l.DB.Query("SELECT date, balance FROM daily_equity ORDER BY date ASC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []map[string]interface{}
	for rows.Next() {
		var date string
		var balance float64
		rows.Scan(&date, &balance)
		data = append(data, map[string]interface{}{"time": date, "val": balance})
	}
	return data, nil
}
func (l *Ledger) SaveFill(symbol, side string, price, qty, pnl float64) error {
	_, err := l.DB.Exec(`INSERT INTO fill_history (symbol, side, price, qty, pnl, timestamp) 
		VALUES (?, ?, ?, ?, ?, ?)`, symbol, side, price, qty, pnl, time.Now().UnixMilli())
	return err
}

func (l *Ledger) GetFills(limit int) ([]map[string]interface{}, error) {
	rows, err := l.DB.Query("SELECT symbol, side, price, qty, pnl, timestamp FROM fill_history ORDER BY id DESC LIMIT ?", limit)
	if err != nil { return nil, err }
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var sym, side string
		var p, q, pnl float64
		var ts int64
		rows.Scan(&sym, &side, &p, &q, &pnl, &ts)
		results = append(results, map[string]interface{}{
			"symbol": sym, "side": side, "price": p, "qty": q, "pnl": pnl, "time": ts,
		})
	}
	return results, nil
}
