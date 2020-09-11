package rtpengine

import (
	"crypto/sha1"
	"net"

	"fmt"

	kcp "github.com/xtaci/kcp-go"
	"golang.org/x/crypto/pbkdf2"
	"webrtc/webrtc/ion-sfu/pkg/log"
	"webrtc/webrtc/ion-sfu/pkg/rtc/rtpengine/udp"
	"webrtc/webrtc/ion-sfu/pkg/rtc/transport"
)

const (
	maxRtpConnSize = 1024
)

var (
	listener    net.Listener
	kcpListener *kcp.Listener
	stop        bool
)

// Serve listen on a port and accept udp conn
// func Serve(port int) chan *udp.Conn {
func Serve(port int) (chan *transport.RTPTransport, error) {
	log.Logger.Infof("rtpengine.Serve port=%d ", port)
	if listener != nil {
		listener.Close()
	}
	ch := make(chan *transport.RTPTransport, maxRtpConnSize)
	var err error
	//建立UDP监听
	listener, err = udp.Listen("udp", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		log.Logger.Errorf("failed to listen %v", err)
		return nil, err
	}
	log.Logger.Debugf("Server rtp with udp: %+v\n", net.UDPAddr{IP: net.IPv4zero, Port: port})

	go func() {
		for {
			if stop {
				return
			}
			//接收连接后，将RTPTransport信息发送到chan
			conn, err := listener.Accept()
			if err != nil {
				log.Logger.Errorf("failed to accept conn %v", err)
				continue
			}
			log.Logger.Infof("accept new rtp conn %s", conn.RemoteAddr().String())

			ch <- transport.NewRTPTransport(conn)
			log.Logger.Debugf("send rtp conn %s to channel\n", conn.RemoteAddr().String())
		}
	}()
	return ch, nil
}

// ServeWithKCP accept kcp conn
func ServeWithKCP(port int, kcpPwd, kcpSalt string) (chan *transport.RTPTransport, error) {
	log.Logger.Infof("kcp Serve port=%d", port)
	if kcpListener != nil {
		kcpListener.Close()
	}
	ch := make(chan *transport.RTPTransport, maxRtpConnSize)
	var err error
	key := pbkdf2.Key([]byte(kcpPwd), []byte(kcpSalt), 1024, 32, sha1.New)
	block, _ := kcp.NewAESBlockCrypt(key)
	kcpListener, err = kcp.ListenWithOptions(fmt.Sprintf("0.0.0.0:%d", port), block, 10, 3)
	if err != nil {
		log.Logger.Errorf("kcp Listen err=%v", err)
		return nil, err
	}

	go func() {
		for {
			if stop {
				return
			}
			conn, err := kcpListener.AcceptKCP()
			if err != nil {
				log.Logger.Errorf("failed to accept conn %v", err)
				continue
			}
			log.Logger.Infof("accept new kcp conn %s", conn.RemoteAddr().String())

			ch <- transport.NewRTPTransport(conn)
		}
	}()
	return ch, nil
}

// Close closes the rtp listener and stops accepting new connections.
func Close() {
	if !stop {
		return
	}
	stop = true
	listener.Close()
	kcpListener.Close()
}
