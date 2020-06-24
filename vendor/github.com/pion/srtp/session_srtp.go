package srtp

import (
	"errors"
	"fmt"
	"net"

	"github.com/pion/logging"
	"github.com/pion/rtp"
)

const defaultSessionSRTPReplayProtectionWindow = 64

// SessionSRTP implements io.ReadWriteCloser and provides a bi-directional SRTP session
// SRTP itself does not have a design like this, but it is common in most applications
// for local/remote to each have their own keying material. This provides those patterns
// instead of making everyone re-implement
type SessionSRTP struct {
	session
	writeStream *WriteStreamSRTP
}

// NewSessionSRTP creates a SRTP session using conn as the underlying transport.
func NewSessionSRTP(conn net.Conn, config *Config) (*SessionSRTP, error) {
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
			SRTPReplayProtection(defaultSessionSRTPReplayProtectionWindow),
		},
		config.RemoteOptions...,
	)

	s := &SessionSRTP{
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
	s.writeStream = &WriteStreamSRTP{s}
	//开始session，循环接收数据、解密数据并写到ReadStreamSRTP的buffer中
	//注: ReadStreamSRTP是session上层的一个stream逻辑，底层连接负责接收数据，
	//经过解密后成为可读性的数据，之后才写到上层的ReadStreamSRTP的buffer中，初始申请大小为1M
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
func (s *SessionSRTP) OpenWriteStream() (*WriteStreamSRTP, error) {
	return s.writeStream, nil
}

// OpenReadStream opens a read stream for the given SSRC, it can be used
// if you want a certain SSRC, but don't want to wait for AcceptStream
func (s *SessionSRTP) OpenReadStream(SSRC uint32) (*ReadStreamSRTP, error) {
	r, _ := s.session.getOrCreateReadStream(SSRC, s, newReadStreamSRTP)

	if readStream, ok := r.(*ReadStreamSRTP); ok {
		return readStream, nil
	}

	return nil, fmt.Errorf("failed to open ReadStreamSRCTP, type assertion failed")
}

// AcceptStream returns a stream to handle RTCP for a single SSRC
func (s *SessionSRTP) AcceptStream() (*ReadStreamSRTP, uint32, error) {
	stream, ok := <-s.newStream
	if !ok {
		return nil, 0, fmt.Errorf("SessionSRTP has been closed")
	}

	readStream, ok := stream.(*ReadStreamSRTP)
	if !ok {
		return nil, 0, fmt.Errorf("newStream was found, but failed type assertion")
	}

	return readStream, stream.GetSSRC(), nil
}

// Close ends the session
func (s *SessionSRTP) Close() error {
	return s.session.close()
}

func (s *SessionSRTP) write(b []byte) (int, error) {
	packet := &rtp.Packet{}

	err := packet.Unmarshal(b)
	if err != nil {
		return 0, nil
	}

	return s.writeRTP(&packet.Header, packet.Payload)
}

func (s *SessionSRTP) writeRTP(header *rtp.Header, payload []byte) (int, error) {
	if _, ok := <-s.session.started; ok {
		return 0, fmt.Errorf("started channel used incorrectly, should only be closed")
	}

	s.session.localContextMutex.Lock()
	//使用localContext加密数据
	encrypted, err := s.localContext.encryptRTP(nil, header, payload)
	s.session.localContextMutex.Unlock()

	if err != nil {
		return 0, err
	}
	//将加密后的数据发送
	return s.session.nextConn.Write(encrypted)
}

func (s *SessionSRTP) decrypt(buf []byte) error {
	h := &rtp.Header{}
	//将数据Unmarshal到RTP数据帧,此处主要获取RTP Header，如果有拓展头，还包括扩展头
	if err := h.Unmarshal(buf); err != nil {
		return err
	}
	//获取上层对应ssrc的Stream对象
	r, isNew := s.session.getOrCreateReadStream(h.SSRC, s, newReadStreamSRTP)
	if r == nil {
		return nil // Session has been closed
	} else if isNew {
		s.session.newStream <- r // Notify AcceptStream
	}

	readStream, ok := r.(*ReadStreamSRTP)
	if !ok {
		return fmt.Errorf("failed to get/create ReadStreamSRTP")
	}
	//使用remoteContext解密收到的数据
	decrypted, err := s.remoteContext.decryptRTP(buf, buf, h)
	if err != nil {
		return err
	}
	//将解密后的数据回写到Stream对象
	_, err = readStream.write(decrypted)
	if err != nil {
		return err
	}

	return nil
}
