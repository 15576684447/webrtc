package log

import (
	"fmt"
	"github.com/pion/logging"
)

var Logger logging.LeveledLogger

type customLogger struct {
}

// Print all messages except trace
func (c customLogger) Trace(msg string)                          {}
func (c customLogger) Tracef(format string, args ...interface{}) {}

func (c customLogger) Debug(msg string) { fmt.Printf("customLogger Debug: %s\n", msg) }
func (c customLogger) Debugf(format string, args ...interface{}) {
	c.Debug(fmt.Sprintf(format, args...))
}
func (c customLogger) Info(msg string) { fmt.Printf("customLogger Info: %s\n", msg) }
func (c customLogger) Infof(format string, args ...interface{}) {
	c.Trace(fmt.Sprintf(format, args...))
}
func (c customLogger) Warn(msg string) { fmt.Printf("customLogger Warn: %s\n", msg) }
func (c customLogger) Warnf(format string, args ...interface{}) {
	c.Warn(fmt.Sprintf(format, args...))
}
func (c customLogger) Error(msg string) { fmt.Printf("customLogger Error: %s\n", msg) }
func (c customLogger) Errorf(format string, args ...interface{}) {
	c.Error(fmt.Sprintf(format, args...))
}

// customLoggerFactory satisfies the interface logging.LoggerFactory
// This allows us to create different loggers per subsystem. So we can
// add custom behavior
type customLoggerFactory struct {
}

func (c customLoggerFactory) NewLogger(subsystem string) logging.LeveledLogger {
	fmt.Printf("Creating logger for %s \n", subsystem)
	return customLogger{}
}

type SfuLogger struct {
	LoggerFactory logging.LoggerFactory
}

// Init initializes the package logger.
// Supported levels are: ["debug", "info", "warn", "error"]
func Init(level string) {
	sfuLogger := SfuLogger{
		LoggerFactory:customLoggerFactory{},
	}
	Logger = sfuLogger.LoggerFactory.NewLogger("sfu")
}
