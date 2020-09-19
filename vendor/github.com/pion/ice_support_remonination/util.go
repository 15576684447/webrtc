package ice

import (
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

var (
	defaultPanicHandler PanicHandler
)

type atomicError struct{ v atomic.Value }

type PanicHandler interface {
	LogPanicStack(s string)
}

func (a *atomicError) Store(err error) {
	a.v.Store(struct{ error }{err})
}
func (a *atomicError) Load() error {
	err, _ := a.v.Load().(struct{ error })
	return err.error
}

// The conditions of invalidation written below are defined in
// https://tools.ietf.org/html/rfc8445#section-5.1.1.1
func isSupportedIPv6(ip net.IP) bool {
	if len(ip) != net.IPv6len ||
		!isZeros(ip[0:12]) || // !(IPv4-compatible IPv6)
		ip[0] == 0xfe && ip[1]&0xc0 == 0xc0 || // !(IPv6 site-local unicast)
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() {
		return false
	}
	return true
}

func isZeros(ip net.IP) bool {
	for i := 0; i < len(ip); i++ {
		if ip[i] != 0 {
			return false
		}
	}
	return true
}

var randmu sync.Mutex
var globalRand *rand.Rand

func init() {
	globalRand = rand.New(rand.NewSource(time.Now().UnixNano()))
}

// RandSeq generates a random alpha numeric sequence of the requested length
func randSeq(n int) string {
	letters := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	b := make([]rune, n)
	randmu.Lock()
	for i := range b {
		b[i] = letters[globalRand.Intn(len(letters))]
	}
	randmu.Unlock()
	return string(b)
}

func parseAddr(in net.Addr) (net.IP, int, NetworkType, bool) {
	switch addr := in.(type) {
	case *net.UDPAddr:
		return addr.IP, addr.Port, NetworkTypeUDP4, true
	case *net.TCPAddr:
		return addr.IP, addr.Port, NetworkTypeTCP4, true
	}
	return nil, 0, 0, false
}

func addrEqual(a, b net.Addr) bool {
	aIP, aPort, aType, aOk := parseAddr(a)
	if !aOk {
		return false
	}

	bIP, bPort, bType, bOk := parseAddr(b)
	if !bOk {
		return false
	}

	return aType == bType && aIP.Equal(bIP) && aPort == bPort
}

func generateCandidateID() (string, error) {
	return generateRandString("candidate:", "")
}

func generateRandString(prefix, sufix string) (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b) //nolint

	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s%X-%X-%X-%X-%X%s", prefix, b[0:4], b[4:6], b[6:8], b[8:10], b[10:], sufix), nil
}

func IsValidPointerInterface(input interface{}) bool {
	return input != nil && !reflect.ValueOf(input).IsNil()
}

func RegisterPanicHandler(h PanicHandler) {
	defaultPanicHandler = h
}

func CheckPanic() {
	if err := recover(); err != nil {
		stack := string(debug.Stack())
		if IsValidPointerInterface(defaultPanicHandler) {
			defaultPanicHandler.LogPanicStack(stack)
		}
	}
}
