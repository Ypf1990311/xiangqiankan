package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", "./alphahft.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT id, symbol, side, price, qty, remaining, timestamp FROM trade_chunks WHERE remaining > 0 ORDER BY side, id DESC LIMIT 50")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	fmt.Printf("=== SQLite Trade Chunks (remaining > 0) ===\n")
	count := 0
	for rows.Next() {
		var id int
		var sym, side string
		var p, q, r float64
		var ts int64
		rows.Scan(&id, &sym, &side, &p, &q, &r, &ts)
		fmt.Printf("[%d] %s %s - Price: %.2f, Qty: %.2f, Rem: %.2f (ts: %d)\n", id, sym, side, p, q, r, ts)
		count++
	}
	fmt.Printf("Total active chunks: %d\n", count)
}
