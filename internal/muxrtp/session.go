package muxrtp

import (
	"fmt"
	"io"
	"net"
	"sync"
)

const (
	receiveMTU = 8192
)

type streamSession interface {
	Close() error
	write([]byte) (int, error)
	handle([]byte) error
}

type session struct {
	newStream chan readStream

	started chan interface{}
	closed  chan interface{}

	readStreamsClosed bool
	readStreams       map[uint32]readStream
	readStreamsLock   sync.Mutex

	nextConn net.Conn
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

func (s *session) start(child streamSession) error {

	go func() {
		defer func() {
			close(s.newStream)

			s.readStreamsLock.Lock()
			s.readStreamsClosed = true
			s.readStreamsLock.Unlock()
			close(s.closed)
		}()

		b := make([]byte, receiveMTU)
		for {
			i, err := s.nextConn.Read(b)
			if err != nil {
				if err != io.EOF {
					fmt.Printf("ERROR: s.nextConn.Read => %s", err.Error())
				}
				return
			}
			if i == 0 {
				fmt.Print("WARNING: s.nextConn.Read = 0")
				continue
			}
			if err = child.handle(b[:i]); err != nil {
				fmt.Printf("WARNING: session.start %v", err)
			}
		}
	}()

	close(s.started)

	return nil
}
