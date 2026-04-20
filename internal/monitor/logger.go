package monitor

import (
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// InitLogger 初始化全局的 zerolog 实例
func InitLogger() {
	// 设置微秒/纳秒级时间格式对高频非常重要
	zerolog.TimeFieldFormat = time.RFC3339Nano
	
	// 在终端友好的输出格式
	consoleWriter := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05.000",
	}
	// With().Caller() 帮助我们在日志中输出代码所在的行号，方便调试
	log.Logger = log.Output(consoleWriter).With().Caller().Logger()
}
