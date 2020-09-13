package log

import (
	"fmt"
	"github.com/pion/logging"
)

type customLogger struct {
	logType string
}

// Print all messages except trace
func (c customLogger) Trace(msg string) { fmt.Printf("%s customLogger Trace: %s\n", c.logType, msg) }
func (c customLogger) Tracef(format string, args ...interface{}) {
	c.Trace(fmt.Sprintf(format, args...))
}

func (c customLogger) Debug(msg string) { fmt.Printf("%s customLogger Debug: %s\n", c.logType, msg) }
func (c customLogger) Debugf(format string, args ...interface{}) {
	c.Debug(fmt.Sprintf(format, args...))
}

func (c customLogger) Info(msg string) { fmt.Printf("%s customLogger Info: %s\n", c.logType, msg) }
func (c customLogger) Infof(format string, args ...interface{}) {
	c.Info(fmt.Sprintf(format, args...))
}

func (c customLogger) Warn(msg string) { fmt.Printf("%s customLogger Warn: %s\n", c.logType, msg) }
func (c customLogger) Warnf(format string, args ...interface{}) {
	c.Warn(fmt.Sprintf(format, args...))
}

func (c customLogger) Error(msg string) { fmt.Printf("%s customLogger Error: %s\n", c.logType, msg) }
func (c customLogger) Errorf(format string, args ...interface{}) {
	c.Error(fmt.Sprintf(format, args...))
}

// customLoggerFactory satisfies the interface logging.LoggerFactory
// This allows us to create different loggers per subsystem. So we can
// add custom behavior
type CustomLoggerFactory struct {
}

func (c CustomLoggerFactory) NewLogger(subsystem string) logging.LeveledLogger {
	fmt.Printf("Creating logger for %s \n", subsystem)
	return customLogger{logType: subsystem}
}

func InitLogger(name string) logging.LeveledLogger {
	sfuLogger := SfuLogger{
		LoggerFactory:CustomLoggerFactory{},
	}

	return sfuLogger.LoggerFactory.NewLogger(name)
}