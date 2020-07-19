package util

import (
	"runtime"
	"runtime/debug"

	"webrtc/webrtc/ion-sfu/pkg/log"
)

func Recover(flag string) {
	_, _, l, _ := runtime.Caller(1)
	if err := recover(); err != nil {
		log.Logger.Errorf("[%s] Recover panic line => %v", flag, l)
		log.Logger.Errorf("[%s] Recover err => %v", flag, err)
		debug.PrintStack()
	}
}
