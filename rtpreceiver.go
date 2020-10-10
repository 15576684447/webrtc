// +build !js

package webrtc

import (
	"fmt"
	"io"
	"sync"
	"webrtc/webrtc/internal/muxrtp"

	"github.com/pion/rtcp"
	"github.com/pion/srtp"
)

// RTPReceiver allows an application to inspect the receipt of a Track
//transport对象中存放的是传输相关的成员，如rtp/rtcp session，conn对象等
//rtpReadStream/rtcpReadStream则是对饮session的流对象，存放最终解密后的数据
type RTPReceiver struct {
	kind           RTPCodecType
	transport      *DTLSTransport
	DisableEncrypt bool

	track *Track

	closed, received chan interface{}
	mu               sync.RWMutex
	//rtp/rtcp需要加密时使用
	rtpReadStream  *srtp.ReadStreamSRTP
	rtcpReadStream *srtp.ReadStreamSRTCP
	//rtp/rtcp无需加密时使用
	muxRtpReadStream  *muxrtp.ReadStreamRTP
	muxRtcpReadStream *muxrtp.ReadStreamRTCP

	// A reference to the associated api object
	api *API
}

// NewRTPReceiver constructs a new RTPReceiver
func (api *API) NewRTPReceiver(kind RTPCodecType, transport *DTLSTransport) (*RTPReceiver, error) {
	if transport == nil {
		return nil, fmt.Errorf("DTLSTransport must not be nil")
	}

	return &RTPReceiver{
		kind:      kind,
		transport: transport,
		api:       api,
		closed:    make(chan interface{}),
		received:  make(chan interface{}),
	}, nil
}

// Transport returns the currently-configured *DTLSTransport or nil
// if one has not yet been configured
func (r *RTPReceiver) Transport() *DTLSTransport {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.transport
}

// Track returns the RTCRtpTransceiver track
func (r *RTPReceiver) Track() *Track {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.track
}

// Receive initialize the track and starts all the transports
//todo: 修改了Receive逻辑，同时支持rtp/rtcp加密与不加密模式
func (r *RTPReceiver) Receive(parameters RTPReceiveParameters) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.received:
		return fmt.Errorf("Receive has already been called")
	default:
	}
	defer close(r.received)

	r.track = &Track{
		kind:     r.kind,
		ssrc:     parameters.Encodings.SSRC,
		receiver: r,
	}
	//加密开关
	r.DisableEncrypt = parameters.DisableEncrypt

	if parameters.DisableEncrypt { //无需dtls加密，直接传输rtp/rtcp
		var err error
		r.transport.rtpSession, err = muxrtp.NewSessionRTP(r.transport.rtpEndpoint)
		if err != nil {
			return fmt.Errorf("muxrtp.NewSessionRTP => %s", err.Error())
		}
		r.muxRtpReadStream, err = r.transport.rtpSession.OpenReadStream(parameters.Encodings.SSRC)
		if err != nil {
			return err
		}
		r.transport.rtcpSession, err = muxrtp.NewSessionRTCP(r.transport.rtcpEndpoint)
		if err != nil {
			return fmt.Errorf("muxrtp.NewSessionRTCP => %s", err.Error())
		}
		r.muxRtcpReadStream, err = r.transport.rtcpSession.OpenReadStream(parameters.Encodings.SSRC)
		if err != nil {
			return err
		}
	} else { //需要dtls加密，传输srtp/srtcp
		//获取RTPReceiver的SRTPSession/SRTCPSession
		//每个session对应若干个stream对象，stream对象使用ssrc区分
		//session收到裸数据后，经过解密，将可读性数据写入对应ssrc的stream对象的buffer中，供应用层使用
		//所以Receive调用后，最终数据会在RTP/RTCP Stream对象的buffer中
		srtpSession, err := r.transport.getSRTPSession()
		if err != nil {
			return err
		}

		r.rtpReadStream, err = srtpSession.OpenReadStream(parameters.Encodings.SSRC)
		if err != nil {
			return err
		}

		srtcpSession, err := r.transport.getSRTCPSession()
		if err != nil {
			return err
		}

		r.rtcpReadStream, err = srtcpSession.OpenReadStream(parameters.Encodings.SSRC)
		if err != nil {
			return err
		}
	}
	return nil
}

// Read reads incoming RTCP for this RTPReceiver
func (r *RTPReceiver) Read(b []byte) (n int, err error) {
	select {
	case <-r.received:
		//todo:同时支持加密与不加密
		if r.DisableEncrypt {
			return r.muxRtcpReadStream.Read(b)
		}
		return r.rtcpReadStream.Read(b)
	case <-r.closed:
		return 0, io.ErrClosedPipe
	}
}

// ReadRTCP is a convenience method that wraps Read and unmarshals for you
func (r *RTPReceiver) ReadRTCP() ([]rtcp.Packet, error) {
	b := make([]byte, receiveMTU)
	i, err := r.Read(b)
	if err != nil {
		return nil, err
	}

	return rtcp.Unmarshal(b[:i])
}

func (r *RTPReceiver) haveReceived() bool {
	select {
	case <-r.received:
		return true
	default:
		return false
	}
}

// Stop irreversibly stops the RTPReceiver
func (r *RTPReceiver) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	select {
	case <-r.closed:
		return nil
	default:
	}

	select {
	case <-r.received:
		if r.rtcpReadStream != nil {
			if err := r.rtcpReadStream.Close(); err != nil {
				return err
			}
		}
		if r.rtpReadStream != nil {
			if err := r.rtpReadStream.Close(); err != nil {
				return err
			}
		}
		//todo:同时支持加密与不加密
		if r.muxRtpReadStream != nil {
			if err := r.muxRtpReadStream.Close(); err != nil {
				return err
			}
		}
		if r.muxRtcpReadStream != nil {
			if err := r.muxRtcpReadStream.Close(); err != nil {
				return err
			}
		}
	default:
	}

	close(r.closed)
	return nil
}

// readRTP should only be called by a track, this only exists so we can keep state in one place
//todo:同时支持加密与不加密
func (r *RTPReceiver) readRTP(b []byte) (n int, err error) {
	<-r.received
	if r.DisableEncrypt {
		return r.muxRtpReadStream.Read(b)
	}
	return r.rtpReadStream.Read(b)
}
