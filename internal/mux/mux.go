// Package mux multiplexes packets on a single socket (RFC7983)
package mux

import (
	"net"
	"sync"

	"github.com/pion/logging"
	"github.com/pion/transport/packetio"
)

// The maximum amount of data that can be buffered before returning errors.
const maxBufferSize = 1000 * 1000 // 1MB

// Config collects the arguments to mux.Mux construction into
// a single structure
type Config struct {
	Conn          net.Conn
	BufferSize    int
	LoggerFactory logging.LoggerFactory
}

// Mux allows multiplexing
type Mux struct {
	lock       sync.RWMutex
	nextConn   net.Conn
	endpoints  map[*Endpoint]MatchFunc
	bufferSize int
	closedCh   chan struct{}

	log logging.LeveledLogger
}

// NewMux creates a new Mux
func NewMux(config Config) *Mux {
	m := &Mux{
		nextConn:   config.Conn,
		endpoints:  make(map[*Endpoint]MatchFunc),
		bufferSize: config.BufferSize,
		closedCh:   make(chan struct{}),
		log:        config.LoggerFactory.NewLogger("mux"),
	}
	//从对端endpoints读取数据，并存放到对应endpoint的buffer中
	//TODO: 多路复用连接上接收到的数据，根据匹配的数据包类型，写入到对应Endpoint的buffer中，并通知上层应用来读取
	go m.readLoop()

	return m
}

// NewEndpoint creates a new Endpoint
func (m *Mux) NewEndpoint(f MatchFunc) *Endpoint {
	e := &Endpoint{
		mux:    m,
		buffer: packetio.NewBuffer(),
	}

	// Set a maximum size of the buffer in bytes.
	// NOTE: We actually won't get anywhere close to this limit.
	// SRTP will constantly read from the endpoint and drop packets if it's full.
	e.buffer.SetLimitSize(maxBufferSize)

	m.lock.Lock()
	m.endpoints[e] = f
	m.lock.Unlock()

	return e
}

// RemoveEndpoint removes an endpoint from the Mux
func (m *Mux) RemoveEndpoint(e *Endpoint) {
	m.lock.Lock()
	defer m.lock.Unlock()
	delete(m.endpoints, e)
}

// Close closes the Mux and all associated Endpoints.
func (m *Mux) Close() error {
	m.lock.Lock()
	for e := range m.endpoints {
		err := e.close()
		if err != nil {
			return err
		}

		delete(m.endpoints, e)
	}
	m.lock.Unlock()

	err := m.nextConn.Close()
	if err != nil {
		return err
	}

	// Wait for readLoop to end
	<-m.closedCh

	return nil
}

func (m *Mux) readLoop() {
	defer func() {
		close(m.closedCh)
	}()

	buf := make([]byte, m.bufferSize)
	for {
		n, err := m.nextConn.Read(buf)
		if err != nil {
			return
		}

		err = m.dispatch(buf[:n])
		if err != nil {
			return
		}
	}
}

func (m *Mux) dispatch(buf []byte) error {
	var endpoint *Endpoint

	m.lock.Lock()
	for e, f := range m.endpoints {
		//f返回的是包匹配器，EndPoint在注册时，会传入一个匹配器，用于匹配不同类型数据包对应的格式格式
		//如果buf中的数据匹配上了某个EndPoint的格式，则将数据存入对应EndPoint中
		if f(buf) {
			endpoint = e
			break
		}
	}
	m.lock.Unlock()

	if endpoint == nil {
		if len(buf) > 0 {
			m.log.Warnf("Warning: mux: no endpoint for packet starting with %d\n", buf[0])
		} else {
			m.log.Warnf("Warning: mux: no endpoint for zero length packet")
		}
		return nil
	}
	//写入到Endpoint buffer，并通知上层来读取
	_, err := endpoint.buffer.Write(buf)
	if err != nil {
		return err
	}

	return nil
}
