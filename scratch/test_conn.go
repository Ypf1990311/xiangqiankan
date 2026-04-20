package main

import (
	"context"
	"fmt"
	"time"

	binance "github.com/adshao/go-binance/v2"
)


func main() {
	// 使用你最新的 Key
	apiKey := "qgEllBsjgzi2i6093miyNs1T8U0xQX0ZaqeLeh5BPrcqXBYsLMso2qegeH6tGqoL"
	secret := "MsyqUW1YrTG4TGA1yNg5zWoREq9elL3S2nolWpBjnUuCyUIpcENuJUZH8XYakbYu"

	client := binance.NewFuturesClient(apiKey, secret)
	ctx := context.Background()

	fmt.Println("🔍 正在执行 AlphaHFT 全链路体检...")

	// 1. Ping
	fmt.Print("1. 基础连通性测试 (Ping)... ")
	err := client.NewPingService().Do(ctx)
	if err != nil {
		fmt.Printf("❌ 失败: %v\n", err)
	} else {
		fmt.Println("✅ 正常")
	}

	// 2. Server Time
	fmt.Print("2. 服务器时间同步检查... ")
	serverTime, err := client.NewServerTimeService().Do(ctx)
	if err != nil {
		fmt.Printf("❌ 失败: %v\n", err)
	} else {
		localTime := time.Now().UnixMilli()
		diff := localTime - serverTime
		fmt.Printf("✅ 延迟: %d ms (阈值 1000ms)\n", diff)
	}

	// 3. Account Permission Check
	fmt.Print("3. API 权限与资产深度扫描 (GetAccount)... \n")
	acc, err := client.NewGetAccountService().Do(ctx)
	if err != nil {
		fmt.Printf("   🚨 鉴权失败！错误信息: %v\n", err)
	} else {
		found := false
		for _, asset := range acc.Assets {
			balance := asset.WalletBalance
			if balance != "0" && balance != "0.00000000" {
				fmt.Printf("   💰 资产: %-6s | 余额: %s | 可用: %s\n", asset.Asset, asset.WalletBalance, asset.AvailableBalance)
				found = true
			}
		}
		if !found {
			fmt.Println("   ⚠️ 未在账户中发现任何有价值的资产。")
		}
		fmt.Println("\n   👉 [诊断结论]: 以上是你程序目前能“看到”的所有钱。")
	}
}

