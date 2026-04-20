package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

// TelegramNotifier 负责向用户的 Telegram 推送警报和核心指标信息
type TelegramNotifier struct {
	Token  string
	ChatID string
	Client *http.Client
}

func NewTelegramNotifier(token, chatID string) *TelegramNotifier {
	return &TelegramNotifier{
		Token:  token,
		ChatID: chatID,
		Client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Send 以异步方式发送消息，避免阻塞主交易引擎
func (t *TelegramNotifier) Send(message string) {
	if t.Token == "" || t.ChatID == "" {
		return // 若未配置则静默跳过
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.Token)
	payload := map[string]string{
		"chat_id":    t.ChatID,
		"text":       "🤖 [AlphaHFT] \n" + message,
		"parse_mode": "HTML",
	}
	data, _ := json.Marshal(payload)

	go func() {
		req, _ := http.NewRequest("POST", url, bytes.NewBuffer(data))
		req.Header.Set("Content-Type", "application/json")

		resp, err := t.Client.Do(req)
		if err != nil {
			log.Error().Err(err).Msg("⚠️ Telegram 消息发送失败(网络错误)")
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			log.Warn().Int("status", resp.StatusCode).Msg("⚠️ Telegram 消息被拒绝 (可能是Token/ChatID错误)")
		}
	}()
}
