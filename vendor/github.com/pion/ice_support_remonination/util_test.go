package ice_support_remonination

import (
	"net"
	"reflect"
	"regexp"
	"testing"
)

func TestRandSeq(t *testing.T) {
	if len(randSeq(10)) != 10 {
		t.Errorf("randSeq return invalid length")
	}

	var isLetter = regexp.MustCompile(`^[a-zA-Z]+$`).MatchString
	if !isLetter(randSeq(10)) {
		t.Errorf("randSeq should be AlphaNumeric only")
	}
}

func Test_isSupportedIPv6(t *testing.T) {

	byteStr := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01}
	if !isSupportedIPv6(byteStr) {
		t.Errorf("is Not SupportedIPv6() = %v", byteStr)
	}

	byteStr = []byte{0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01}
	if isSupportedIPv6(byteStr) {
		t.Errorf("is SupportedIPv6() = %v", byteStr)
	}

	byteStr = []byte{255, 255, 255, 255}
	if isSupportedIPv6(byteStr) {
		t.Errorf("is SupportedIPv6() = %v", byteStr)
	}
}

func Test_parseAddr(t *testing.T) {
	ip := net.IP{255, 255, 255, 255}
	port := 8888
	addr := &net.UDPAddr{ip, port, "test"}
	ad, po, prot, ok := parseAddr(addr)
	if !reflect.DeepEqual(ad, ip) || po != port || prot != NetworkTypeUDP4 || !ok {
		t.Errorf("parseAddr() = %v, %v, %v , %v", ad, po, prot, ok)
	}

	ip = net.IP{255, 255, 255, 255}
	port = 8888
	addr = &net.UDPAddr{ip, port, "test2"}
	ad, po, prot, ok = parseAddr(addr)
	ip = append(ip, 128)
	if reflect.DeepEqual(ad, ip) || po != port || prot != NetworkTypeUDP4 || !ok {
		t.Errorf("parseAddr() = %v, %v, %v , %v", ad, po, prot, ok)
	}
}

func Test_addrEqual(t *testing.T) {
	ip := net.IP{255, 255, 255, 255}
	port := 8888
	a := &net.UDPAddr{ip, port, "test"}
	b := &net.UDPAddr{ip, port, "test2"}
	if !addrEqual(a, b) {
		t.Errorf("addrEqual() = %v, %v", a, b)
	}
	b = &net.UDPAddr{append(ip, 255), port + 1, "test2"}
	if addrEqual(a, b) {
		t.Errorf("addrEqual() = %v, %v", a, b)
	}
}

type DummyPanicHandler struct {
	panicContext *string
}

func (h *DummyPanicHandler) LogPanicStack(stack string) {
	*h.panicContext = stack
}

func GenPanic() {
	defer CheckPanic()
	//panic("test panic")
	var a *int
	*a = 100
}

func TestPanicHandler(t *testing.T) {
	var context string
	RegisterPanicHandler(&DummyPanicHandler{panicContext: &context})
	GenPanic()
	if len(context) == 0 {
		t.Errorf("can't catch panic")
	}
}
