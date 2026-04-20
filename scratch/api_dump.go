package main

import (
	"context"
	"fmt"
	"log"
	"alphahft/internal/config"
	"alphahft/internal/exchange/binance"
	"encoding/json"
)

func main() {
	cfg, err := config.LoadConfig("config/config.yaml")
	if err != nil {
		log.Fatal(err)
	}

	client := binance.NewFuturesClient(cfg.Exchange.APIKey, cfg.Exchange.SecretKey, false)
	
	fmt.Println("--- Position Risk V2 Dump ---")
	// 我们尝试直接抓取所有持仓信息
	res, err := client.Client.NewGetPositionRiskService().Do(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	for _, p := range res {
		if p.Symbol == "BNBUSDC" {
			b, _ := json.MarshalIndent(p, "", "  ")
			fmt.Println(string(b))
		}
	}

	fmt.Println("\n--- Account V2 Dump ---")
	acc, err := client.Client.NewGetAccountService().Do(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	for _, p := range acc.Positions {
		if p.Symbol == "BNBUSDC" {
			b, _ := json.MarshalIndent(p, "", "  ")
			fmt.Println(string(b))
		}
	}
}
