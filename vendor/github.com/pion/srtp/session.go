package srtp

import (
	"io"
	"net"
	"sync"

	"github.com/pion/logging"
)

type streamSession interface {
	Close() error
	write([]byte) (int, error)
	decrypt([]byte) error
}

type session struct {
	localContextMutex           sync.Mutex
	localContext, remoteContext *Context
	localOptions, remoteOptions []ContextOption

	newStream chan readStream

	started chan interface{}
	closed  chan interface{}

	readStreamsClosed bool
	readStreams       map[uint32]readStream
	readStreamsLock   sync.Mutex

	log logging.LeveledLogger

	nextConn net.Conn
}

// Config is used to configure a session.
// You can provide either a KeyingMaterialExporter to export keys
// or directly pass the keys themselves.
// After a Config is passed to a session it must not be modified.
type Config struct {
	Keys          SessionKeys
	Profile       ProtectionProfile
	LoggerFactory logging.LoggerFactory

	// List of local/remote context options.
	// ReplayProtection is enabled on remote context by default.
	// Default replay protection window size is 64.
	LocalOptions, RemoteOptions []ContextOption
}

// SessionKeys bundles the keys required to setup an SRTP session
type SessionKeys struct {
	LocalMasterKey   []byte
	LocalMasterSalt  []byte
	RemoteMasterKey  []byte
	RemoteMasterSalt []byte
}

func (s *session) getOrCreateReadStream(ssrc uint32, child streamSession, proto func() readStream) (readStream, bool) {
	s.readStreamsLock.Lock()
	defer s.readStreamsLock.Unlock()

	if s.readStreamsClosed {
		return nil, false
	}

	r, ok := s.readStreams[ssrc]
	if ok {
		return r, false
	}

	// Create the readStream.
	r = proto()

	if err := r.init(child, ssrc); err != nil {
		return nil, false
	}

	s.readStreams[ssrc] = r
	return r, true
}

func (s *session) removeReadStream(ssrc uint32) {
	s.readStreamsLock.Lock()
	defer s.readStreamsLock.Unlock()

	if s.readStreamsClosed {
		return
	}

	delete(s.readStreams, ssrc)
}

func (s *session) close() error {
	if s.nextConn == nil {
		return nil
	} else if err := s.nextConn.Close(); err != nil {
		return err
	}

	<-s.closed
	return nil
}

func (s *session) start(localMasterKey, localMasterSalt, remoteMasterKey, remoteMasterSalt []byte, profile ProtectionProfile, child streamSession) error {
	var err error
	//localContext用于发送数据时加密数据
	s.localContext, err = CreateContext(localMasterKey, localMasterSalt, profile, s.localOptions...)
	if err != nil {
		return err
	}
	//remoteContext用于接收数据时解密数据
	s.remoteContext, err = CreateContext(remoteMasterKey, remoteMasterSalt, profile, s.remoteOptions...)
	if err != nil {
		return err
	}

	go func() {
		defer func() {
			close(s.newStream)

			s.readStreamsLock.Lock()
			s.readStreamsClosed = true
			s.readStreamsLock.Unlock()
			close(s.closed)
		}()

		b := make([]byte, 8192)
		for {
			var i int
			//TODO!!! 这里的conn其实是传入的Endpoint，其有Read/Write方法，Endpoint底层封装的多路复用器
			//底层逻辑服务接收裸数据，此时的数据是经过加密的
			i, err = s.nextConn.Read(b)
			if err != nil {
				if err != io.EOF {
					s.log.Errorf("srtp: %s", err.Error())
				}
				return
			}
			//解密数据并回写到上层ReadStreamSRTP的buffer中
			if err = child.decrypt(b[:i]); err != nil {
				s.log.Infof("%v \n", err)
			}
		}
	}()

	close(s.started)

	return nil
}
