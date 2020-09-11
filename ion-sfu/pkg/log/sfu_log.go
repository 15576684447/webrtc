package log

import "github.com/pion/logging"

var Logger logging.LeveledLogger
//此处logging.LoggerFactory为接口类型，任意实现该接口全部方法的对象都可以直接赋值给该接口
//此处自定义了customLoggerFactory对象，实现了logging.LoggerFactory全部方法
type SfuLogger struct {
	LoggerFactory logging.LoggerFactory
}

// Init initializes the package logger.
// Supported levels are: ["debug", "info", "warn", "error"]
func Init(level string) {
	sfuLogger := SfuLogger{
		LoggerFactory:CustomLoggerFactory{},
	}

	Logger = sfuLogger.LoggerFactory.NewLogger("sfu")
}