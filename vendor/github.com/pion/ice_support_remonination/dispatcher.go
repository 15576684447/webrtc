package ice_support_remonination

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/stun"
)

type Dispatcher struct {
	mu     sync.RWMutex
	addrmu sync.RWMutex

	AddrMap    map[string]Candidate
	UFragMap   map[string]Candidate
	ReverseMap map[Candidate][]string

	//if port is used by single port
	usedPort map[string][]net.PacketConn

	rrCount       uint32
	pkgDiscarded  uint64
	pkgWrongStun  uint64
	pkgTransfered uint64
}

var globalDispatcherMap sync.Map

func getDispatcher(localIp string, localport uint16) *Dispatcher {
	value, _ := globalDispatcherMap.LoadOrStore(localIp+":"+strconv.Itoa(int(localport)), createDispatcher())
	return value.(*Dispatcher)
}

func createDispatcher() *Dispatcher {
	var g Dispatcher
	g.pkgDiscarded = 0
	g.pkgTransfered = 0
	g.rrCount = 0
	g.AddrMap = make(map[string]Candidate)
	g.ReverseMap = make(map[Candidate][]string)
	g.UFragMap = make(map[string]Candidate)
	g.usedPort = make(map[string][]net.PacketConn)
	return &g
}

func (d *Dispatcher) RegisterCand(port uint16, addr string, cand, remote Candidate, ctype, attr string) error {

	localUfrag := cand.agent().localUfrag

	d.mu.Lock()
	defer d.mu.Unlock()

	fullAddr := addr + ":" + strconv.Itoa(int(port))

	if len(d.usedPort[fullAddr]) == 0 {
		//goroutine not created
		v := make([]net.PacketConn, 0)
		for i := 0; i < runtime.NumCPU()*2; i++ {
			conn, err := makeReuseListenSocket(fullAddr)
			v = append(v, conn)
			if err != nil {
				return errors.New("create reuse port failed")
			}
			go reuseLoop(d, conn)
		}
		d.usedPort[fullAddr] = v
	}

	if c, ok := d.UFragMap[localUfrag]; ok {
		if c != cand {
			//already have candidate
			fmt.Errorf("ufrag conflict %v. ip from %v", localUfrag, addr)
			return errors.New("ufrag conflict")
		}
	} else {
		d.UFragMap[localUfrag] = cand
	}

	if remote != nil {
		// explicit set remote cand
		addr = remote.addr().String()
		d.addrmu.Lock()
		if _, exist := d.AddrMap[addr]; !exist {
			d.AddrMap[addr] = cand
			reverseArray := d.ReverseMap[cand]
			if reverseArray == nil {
				reverseArray = make([]string, 0)
			}
			reverseArray = append(reverseArray, addr)
			d.ReverseMap[cand] = reverseArray
		}
		d.addrmu.Unlock()
	}

	return nil
}

//unregister will be executed in worker
func (d *Dispatcher) UnRegisterCand(cand Candidate) int {
	localUfrag := cand.agent().localUfrag
	d.mu.Lock()
	if cand == d.UFragMap[localUfrag] {
		delete(d.UFragMap, localUfrag)
	}
	d.mu.Unlock()

	d.addrmu.Lock()
	reverseAddr := d.ReverseMap[cand]
	for i := range reverseAddr {
		delete(d.AddrMap, reverseAddr[i])
	}
	delete(d.ReverseMap, cand)
	d.addrmu.Unlock()

	return 0
}

//get conn for write
func (d *Dispatcher) GetRandomConn(addr string) net.PacketConn {
	rand.Seed(time.Now().Unix())
	d.mu.RLock()
	defer d.mu.RUnlock()
	d.rrCount++
	return d.usedPort[addr][(int)(d.rrCount)%len(d.usedPort[addr])]
}

func reuseLoop(d *Dispatcher, packetConn net.PacketConn) {
	defer CheckPanic()
	buf := make([]byte, receiveMTU)
	for {
		n, srcAddr, err := packetConn.ReadFrom(buf)
		addr := srcAddr.String()
		if err != nil {
			return
		}
		buffer := buf[0:n]

		if stun.IsMessage(buffer) {
			m := &stun.Message{
				Raw: make([]byte, len(buffer)),
			}
			// Explicitly copy raw buffer so Message can own the memory.
			copy(m.Raw, buffer)
			if err := m.Decode(); err != nil {
				atomic.AddUint64(&d.pkgWrongStun, 1)
				continue
			}
			if m.Type.Class == stun.ClassSuccessResponse {
				d.addrmu.RLock()
				if cand, ok := d.AddrMap[addr]; !ok {
					d.addrmu.RUnlock()
					atomic.AddUint64(&d.pkgDiscarded, 1)
				} else {
					d.addrmu.RUnlock()
					cand.agent().run(func(agent *Agent) { // nolint
						agent.handleInbound(m, cand, srcAddr)
					})
				}
				continue
			}
			username, err := getStunUsername(m)

			if err != nil {
				fmt.Printf("get username from ping failed. ip from %v", addr)
				atomic.AddUint64(&d.pkgWrongStun, 1)
				continue
			}
			ufrags := strings.Split(username, ":")
			if len(ufrags) != 2 {
				fmt.Printf("ufrag error. ip from %v", addr)
				continue
			}
			username = ufrags[0]
			d.mu.RLock()
			if cand, ok := d.UFragMap[username]; ok {
				d.mu.RUnlock()
				err := cand.agent().run(func(agent *Agent) {
					agent.handleInbound(m, cand, srcAddr)
				})
				if err != nil {
					continue
				}
				d.addrmu.Lock()
				if _, ok := d.AddrMap[addr]; !ok {
					d.AddrMap[addr] = cand
					reverseArray := d.ReverseMap[cand]
					if reverseArray == nil {
						reverseArray = make([]string, 0)
					}
					reverseArray = append(reverseArray, addr)
					d.ReverseMap[cand] = reverseArray
				}
				d.addrmu.Unlock()

			} else {
				d.mu.RUnlock()
			}

		} else {
			d.addrmu.RLock()
			if cand, ok := d.AddrMap[addr]; !ok {
				d.addrmu.RUnlock()
				atomic.AddUint64(&d.pkgDiscarded, 1)
			} else {
				d.addrmu.RUnlock()
				cand.seen(false)
				log := cand.agent().log

				var n int
				if IsValidPointerInterface(cand.agent().dataHandler) {
					n, err = cand.agent().dataHandler.Process(buffer)
				}
				if n == 0 {
					// NOTE This will return ice.ErrFull if the buffer ever manages to fill up.
					if _, err := cand.agent().buffer.Write(buffer); err != nil {
						log.Warnf("failed to write packet")
					}
				}
			}
		}

	}
}
