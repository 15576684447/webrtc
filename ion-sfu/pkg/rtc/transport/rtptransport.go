package transport

import (
	"crypto/sha1"
	"errors"
	"net"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/xtaci/kcp-go"
	"golang.org/x/crypto/pbkdf2"
	"webrtc/webrtc/ion-sfu/pkg/log"
	"webrtc/webrtc/ion-sfu/pkg/rtc/rtpengine/muxrtp"
	"webrtc/webrtc/ion-sfu/pkg/rtc/rtpengine/muxrtp/mux"
)

const (
	receiveMTU = 1500
	maxPktSize = 1024
)

var (
	errInvalidConn = errors.New("invalid conn")
)

// RTPTransport ..
type RTPTransport struct {
	rtpSession     *muxrtp.SessionRTP
	rtcpSession    *muxrtp.SessionRTCP
	rtpEndpoint    *mux.Endpoint
	rtcpEndpoint   *mux.Endpoint
	conn           net.Conn
	mux            *mux.Mux
	rtpCh          chan *rtp.Packet //pub端接收数据的chan，然后sfu从中读取数据后转发
	ssrcPT         map[uint32]uint8
	ssrcPTLock     sync.RWMutex
	stop           bool
	id             string
	idLock         sync.RWMutex
	writeErrCnt    int
	rtcpCh         chan rtcp.Packet //接收sub端rtcp数据包的chan
	bandwidth      uint32
	IDChan         chan string
	onCloseHandler func()
}

// NewRTPTransport create a RTPTransport by net.Conn
func NewRTPTransport(conn net.Conn) *RTPTransport {
	if conn == nil {
		log.Logger.Errorf("NewRTPTransport err=%v", errInvalidConn)
		return nil
	}
	t := &RTPTransport{
		conn:   conn,
		rtpCh:  make(chan *rtp.Packet, maxPktSize),
		ssrcPT: make(map[uint32]uint8),
		rtcpCh: make(chan rtcp.Packet, maxPktSize),
		IDChan: make(chan string),
	}
	config := mux.Config{
		Conn:       conn,
		BufferSize: receiveMTU,
	}
	//连接复用器，接收数据匹配不同的EndPoint，存放到不同的EndPoint下，即区分RTP和RTCP
	t.mux = mux.NewMux(config)
	t.rtpEndpoint = t.newEndpoint(mux.MatchRTP)
	t.rtcpEndpoint = t.newEndpoint(mux.MatchRTCP)
	var err error
	//创建rtpSession和rtcpSession，生成rtpStream和rtcpStream
	//stream从EndPoint读取数据，根据不同ssrc，存放到不同buffer，供应用层使用
	t.rtpSession, err = muxrtp.NewSessionRTP(t.rtpEndpoint)
	if err != nil {
		log.Logger.Errorf("muxrtp.NewSessionRTP => %s", err.Error())
		return nil
	}
	t.rtcpSession, err = muxrtp.NewSessionRTCP(t.rtcpEndpoint)
	if err != nil {
		log.Logger.Errorf("muxrtp.NewSessionRTCP => %s", err.Error())
		return nil
	}
	//读取rtp流
	t.receiveRTP()
	return t
}

// NewOutRTPTransport new a outgoing RTPTransport
func NewOutRTPTransport(id, addr string) *RTPTransport {
	srcAddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	dstAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Logger.Errorf("net.ResolveUDPAddr => %s", err.Error())
		return nil
	}
	conn, err := net.DialUDP("udp", srcAddr, dstAddr)
	if err != nil {
		log.Logger.Errorf("net.DialUDP => %s", err.Error())
		return nil
	}
	r := NewRTPTransport(conn)
	r.receiveRTCP()
	log.Logger.Infof("NewOutRTPTransport %s %s", id, addr)
	r.idLock.Lock()
	defer r.idLock.Unlock()
	r.id = id
	return r
}

// NewOutRTPTransportWithKCP  new a outgoing RTPTransport by kcp
func NewOutRTPTransportWithKCP(id, addr string, kcpKey, kcpSalt string) *RTPTransport {
	key := pbkdf2.Key([]byte(kcpKey), []byte(kcpSalt), 1024, 32, sha1.New)
	block, _ := kcp.NewAESBlockCrypt(key)

	// dial to the echo server
	conn, err := kcp.DialWithOptions(addr, block, 10, 3)
	if err != nil {
		log.Logger.Errorf("NewOutRTPTransportWithKCP err=%v", err)
	}
	r := NewRTPTransport(conn)
	r.receiveRTCP()
	log.Logger.Infof("NewOutRTPTransportWithKCP %s %s", id, addr)
	r.idLock.Lock()
	defer r.idLock.Unlock()
	r.id = id
	return r
}

// ID return id
func (r *RTPTransport) ID() string {
	r.idLock.RLock()
	defer r.idLock.RUnlock()
	return r.id
}

// Type return type of transport
func (r *RTPTransport) Type() int {
	return TypeRTPTransport
}

// Close release all
func (r *RTPTransport) Close() {
	if r.stop {
		return
	}
	log.Logger.Infof("RTPTransport.Close()")
	r.stop = true
	r.rtpSession.Close()
	r.rtcpSession.Close()
	r.rtpEndpoint.Close()
	r.rtcpEndpoint.Close()
	r.mux.Close()
	r.conn.Close()
}

// OnClose calls passed handler when closing pc
func (r *RTPTransport) OnClose(f func()) {
	r.onCloseHandler = f
}

// newEndpoint registers a new endpoint on the underlying mux.
func (r *RTPTransport) newEndpoint(f mux.MatchFunc) *mux.Endpoint {
	return r.mux.NewEndpoint(f)
}

// ReceiveRTP receive rtp
func (r *RTPTransport) receiveRTP() {
	go func() {
		for {
			if r.stop {
				break
			}
			//阻塞型，每次有新的stream到来(新的ssrc)，则获取其句柄，并开启一个协程专门用于接收该ssrc对应的stream
			readStream, ssrc, err := r.rtpSession.AcceptStream()
			if err == muxrtp.ErrSessionRTPClosed {
				r.Close()
				return
			} else if err != nil {
				log.Logger.Warnf("Failed to accept stream %v ", err)
				//for non-blocking ReadRTP()
				r.rtpCh <- nil
				continue
			}
			go func() {

				for {
					if r.stop {
						return
					}
					rtpBuf := make([]byte, receiveMTU)
					_, pkt, err := readStream.ReadRTP(rtpBuf)
					if err != nil {
						log.Logger.Warnf("Failed to read rtp %v %d ", err, ssrc)
						//for non-blocking ReadRTP()
						r.rtpCh <- nil
						continue
						// return
					}

					log.Logger.Debugf("RTPTransport.receiveRTP pkt=%v", pkt)
					r.idLock.Lock()
					if r.id == "" {
						ext := pkt.GetExtension(1)
						if ext != nil {
							r.id = string(ext)
							r.IDChan <- r.id
						}
					}
					r.idLock.Unlock()

					r.rtpCh <- pkt
					r.ssrcPTLock.Lock()
					r.ssrcPT[pkt.Header.SSRC] = pkt.Header.PayloadType
					r.ssrcPTLock.Unlock()
					// log.Debugf("got RTP: %+v", pkt.Header)
				}
			}()
		}
	}()
}

// ReadRTP read rtp from transport
func (r *RTPTransport) ReadRTP() (*rtp.Packet, error) {
	return <-r.rtpCh, nil
}

// rtp sub receive rtcp
func (r *RTPTransport) receiveRTCP() {
	go func() {
		for {
			if r.stop {
				break
			}
			//阻塞型，每次有新的stream到来(新的ssrc)，则获取其句柄，并开启一个协程专门用于接收该ssrc对应的stream
			readStream, ssrc, err := r.rtcpSession.AcceptStream()
			if err == muxrtp.ErrSessionRTCPClosed {
				return
			} else if err != nil {
				log.Logger.Warnf("Failed to accept RTCP %v ", err)
				return
			}

			go func() {
				rtcpBuf := make([]byte, receiveMTU)
				for {
					if r.stop {
						return
					}
					rtcps, err := readStream.ReadRTCP(rtcpBuf)
					if err != nil {
						log.Logger.Warnf("Failed to read rtcp %v %d ", err, ssrc)
						return
					}
					log.Logger.Debugf("got RTCPs: %+v ", rtcps)
					for _, pkt := range rtcps {
						switch pkt.(type) {
						case *rtcp.PictureLossIndication:
							log.Logger.Debugf("got pli, not need send key frame!")
						case *rtcp.TransportLayerNack:
							log.Logger.Debugf("rtptransport got nack: %+v", pkt)
							r.rtcpCh <- pkt
						}
					}
				}
			}()
		}
	}()
}

func (r *RTPTransport) setIDHeaderExtension(rtp *rtp.Packet) error {
	err := rtp.SetExtension(1, []byte(r.id))
	if err != nil {
		return err
	}
	return nil
}

// WriteRTP send rtp packet
func (r *RTPTransport) WriteRTP(rtp *rtp.Packet) error {
	log.Logger.Debugf("RTPTransport.WriteRTP rtp=%v", rtp)
	writeStream, err := r.rtpSession.OpenWriteStream()
	if err != nil {
		log.Logger.Errorf("write error %+v", err)
		r.writeErrCnt++
		return err
	}

	pkt := *rtp

	if rtp.SequenceNumber%10 == 0 {
		r.idLock.Lock()
		err := r.setIDHeaderExtension(&pkt)
		if err != nil {
			log.Logger.Errorf("error setting id to rtp extension %+v", err)
		}
		r.idLock.Unlock()
	}

	_, err = writeStream.WriteRTP(&pkt.Header, pkt.Payload)

	if err != nil {
		log.Logger.Errorf("writeStream.WriteRTP => %s", err.Error())
		r.writeErrCnt++
	}
	return err
}

// WriteRawRTCP write rtcp data
func (r *RTPTransport) WriteRawRTCP(data []byte) (int, error) {
	writeStream, err := r.rtcpSession.OpenWriteStream()
	if err != nil {
		return 0, err
	}
	return writeStream.WriteRawRTCP(data)
}

// SSRCPT playload type and ssrc
func (r *RTPTransport) SSRCPT() map[uint32]uint8 {
	r.ssrcPTLock.RLock()
	defer r.ssrcPTLock.RUnlock()
	return r.ssrcPT
}

// WriteRTCP write rtcp
func (r *RTPTransport) WriteRTCP(pkt rtcp.Packet) error {
	bin, err := pkt.Marshal()
	if err != nil {
		return err
	}
	_, err = r.WriteRawRTCP(bin)
	if err != nil {
		return err
	}
	return err
}

// WriteErrTotal return write error
func (r *RTPTransport) WriteErrTotal() int {
	return r.writeErrCnt
}

// WriteErrReset reset write error
func (r *RTPTransport) WriteErrReset() {
	r.writeErrCnt = 0
}

// GetRTCPChan return a rtcp channel
func (r *RTPTransport) GetRTCPChan() chan rtcp.Packet {
	return r.rtcpCh
}

// RemoteAddr return remote addr
func (r *RTPTransport) RemoteAddr() net.Addr {
	if r.conn == nil {
		log.Logger.Errorf("RemoteAddr err=%v", errInvalidConn)
		return nil
	}
	return r.conn.RemoteAddr()
}

// GetBandwidth get bindwitdh setting
func (r *RTPTransport) GetBandwidth() uint32 {
	return r.bandwidth
}
