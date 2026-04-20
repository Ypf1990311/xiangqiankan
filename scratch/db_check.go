package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", "alphahft.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	fmt.Println("Tables in alphahft.db:")
	for rows.Next() {
		var name string
		rows.Scan(&name)
		fmt.Println("-", name)
	}

	var count int
	err = db.QueryRow("SELECT count(*) FROM fill_history").Scan(&count)
	if err != nil {
		fmt.Println("Error querying fill_history:", err)
	} else {
		fmt.Printf("fill_history count: %d\n", count)
	}
}
