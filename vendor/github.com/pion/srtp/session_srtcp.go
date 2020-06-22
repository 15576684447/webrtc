package srtp

import (
	"errors"
	"fmt"
	"net"

	"github.com/pion/logging"
	"github.com/pion/rtcp"
)

const defaultSessionSRTCPReplayProtectionWindow = 64

// SessionSRTCP implements io.ReadWriteCloser and provides a bi-directional SRTCP session
// SRTCP itself does not have a design like this, but it is common in most applications
// for local/remote to each have their own keying material. This provides those patterns
// instead of making everyone re-implement
type SessionSRTCP struct {
	session
	writeStream *WriteStreamSRTCP
}

// NewSessionSRTCP creates a SRTCP session using conn as the underlying transport.
func NewSessionSRTCP(conn net.Conn, config *Config) (*SessionSRTCP, error) {
	if config == nil {
		return nil, errors.New("no config provided")
	} else if conn == nil {
		return nil, errors.New("no conn provided")
	}

	loggerFactory := config.LoggerFactory
	if loggerFactory == nil {
		loggerFactory = logging.NewDefaultLoggerFactory()
	}

	localOpts := append(
		[]ContextOption{},
		config.LocalOptions...,
	)
	remoteOpts := append(
		[]ContextOption{
			// Default options
			SRTCPReplayProtection(defaultSessionSRTCPReplayProtectionWindow),
		},
		config.RemoteOptions...,
	)

	s := &SessionSRTCP{
		session: session{
			nextConn:      conn,
			localOptions:  localOpts,
			remoteOptions: remoteOpts,
			readStreams:   map[uint32]readStream{},
			newStream:     make(chan readStream),
			started:       make(chan interface{}),
			closed:        make(chan interface{}),
			log:           loggerFactory.NewLogger("srtp"),
		},
	}
	s.writeStream = &WriteStreamSRTCP{s}

	err := s.session.start(
		config.Keys.LocalMasterKey, config.Keys.LocalMasterSalt,
		config.Keys.RemoteMasterKey, config.Keys.RemoteMasterSalt,
		config.Profile,
		s,
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// OpenWriteStream returns the global write stream for the Session
func (s *SessionSRTCP) OpenWriteStream() (*WriteStreamSRTCP, error) {
	return s.writeStream, nil
}

// OpenReadStream opens a read stream for the given SSRC, it can be used
// if you want a certain SSRC, but don't want to wait for AcceptStream
func (s *SessionSRTCP) OpenReadStream(SSRC uint32) (*ReadStreamSRTCP, error) {
	r, _ := s.session.getOrCreateReadStream(SSRC, s, newReadStreamSRTCP)

	if readStream, ok := r.(*ReadStreamSRTCP); ok {
		return readStream, nil
	}
	return nil, fmt.Errorf("failed to open ReadStreamSRCTP, type assertion failed")
}

// AcceptStream returns a stream to handle RTCP for a single SSRC
func (s *SessionSRTCP) AcceptStream() (*ReadStreamSRTCP, uint32, error) {
	stream, ok := <-s.newStream
	if !ok {
		return nil, 0, fmt.Errorf("SessionSRTCP has been closed")
	}

	readStream, ok := stream.(*ReadStreamSRTCP)
	if !ok {
		return nil, 0, fmt.Errorf("newStream was found, but failed type assertion")
	}

	return readStream, stream.GetSSRC(), nil
}

// Close ends the session
func (s *SessionSRTCP) Close() error {
	return s.session.close()
}

// Private

func (s *SessionSRTCP) write(buf []byte) (int, error) {
	if _, ok := <-s.session.started; ok {
		return 0, fmt.Errorf("started channel used incorrectly, should only be closed")
	}

	s.session.localContextMutex.Lock()
	//使用localContext加密数据
	encrypted, err := s.localContext.EncryptRTCP(nil, buf, nil)
	s.session.localContextMutex.Unlock()

	if err != nil {
		return 0, err
	}
	//发送加密的数据
	return s.session.nextConn.Write(encrypted)
}

//create a list of Destination SSRCs
//that's a superset of all Destinations in the slice.
func destinationSSRC(pkts []rtcp.Packet) []uint32 {
	ssrcSet := make(map[uint32]struct{})
	for _, p := range pkts {
		for _, ssrc := range p.DestinationSSRC() {
			ssrcSet[ssrc] = struct{}{}
		}
	}

	out := make([]uint32, 0, len(ssrcSet))
	for ssrc := range ssrcSet {
		out = append(out, ssrc)
	}

	return out
}

func (s *SessionSRTCP) decrypt(buf []byte) error {
	//使用remoteContext解密rtcp数据包
	decrypted, err := s.remoteContext.DecryptRTCP(buf, buf, nil)
	if err != nil {
		return err
	}
	//将解密后的数据Unmarshal为rtcp pkt格式
	pkt, err := rtcp.Unmarshal(decrypted)
	if err != nil {
		return err
	}
	//遍历rtcp pkt包中包含的ssrc，找到对应的rtcp stream，将解密后的数据写入到对应rtcp stream对象中供应用层使用
	for _, ssrc := range destinationSSRC(pkt) {
		r, isNew := s.session.getOrCreateReadStream(ssrc, s, newReadStreamSRTCP)
		if r == nil {
			return nil // Session has been closed
		} else if isNew {
			s.session.newStream <- r // Notify AcceptStream
		}

		readStream, ok := r.(*ReadStreamSRTCP)
		if !ok {
			return fmt.Errorf("failed to get/create ReadStreamSRTP")
		}

		_, err = readStream.write(decrypted)
		if err != nil {
			return err
		}
	}

	return nil
}
