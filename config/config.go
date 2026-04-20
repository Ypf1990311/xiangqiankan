package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Exchange ExchangeConfig `yaml:"exchange"`
	Notify   NotifyConfig   `yaml:"notify"`
	Symbols  []string       `yaml:"symbols"`
}

type NotifyConfig struct {
	TelegramToken  string `yaml:"telegram_token"`
	TelegramChatID string `yaml:"telegram_chat_id"`
}

type ExchangeConfig struct {
	Name       string `yaml:"name"`
	Testnet    bool   `yaml:"testnet"`
	PaperTrade bool   `yaml:"paper_trade"`
	APIKey     string `yaml:"api_key"`
	Secret     string `yaml:"secret"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	err = yaml.Unmarshal(b, &cfg)
	return &cfg, err
}
