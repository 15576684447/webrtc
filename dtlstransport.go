// +build !js

package webrtc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"webrtc/webrtc/internal/mux"
	"webrtc/webrtc/internal/muxrtp"
	"webrtc/webrtc/internal/util"
	"webrtc/webrtc/pkg/rtcerr"

	"github.com/pion/dtls/v2"
	"github.com/pion/dtls/v2/pkg/crypto/fingerprint"
	"github.com/pion/srtp"
)

// DTLSTransport allows an application access to information about the DTLS
// transport over which RTP and RTCP packets are sent and received by
// RTPSender and RTPReceiver, as well other data such as SCTP packets sent
// and received by data channels.
type DTLSTransport struct {
	lock sync.RWMutex

	iceTransport      *ICETransport
	certificates      []Certificate
	remoteParameters  DTLSParameters
	remoteCertificate []byte
	state             DTLSTransportState

	onStateChangeHdlr func(DTLSTransportState)

	conn *dtls.Conn
	//开启DTLS加密时，使用srtp/srtcp endpoint
	srtpSession   *srtp.SessionSRTP
	srtcpSession  *srtp.SessionSRTCP
	srtpEndpoint  *mux.Endpoint
	srtcpEndpoint *mux.Endpoint
	//不开启DTLS加密时，使用rtp/rtcp endpoint
	rtpEndpoint  *mux.Endpoint
	rtcpEndpoint *mux.Endpoint
	rtpSession   *muxrtp.SessionRTP
	rtcpSession  *muxrtp.SessionRTCP

	dtlsMatcher mux.MatchFunc

	api *API
}

// NewDTLSTransport creates a new DTLSTransport.
// This constructor is part of the ORTC API. It is not
// meant to be used together with the basic WebRTC API.
func (api *API) NewDTLSTransport(transport *ICETransport, certificates []Certificate) (*DTLSTransport, error) {
	t := &DTLSTransport{
		iceTransport: transport,
		api:          api,
		state:        DTLSTransportStateNew,
		dtlsMatcher:  mux.MatchDTLS,
	}

	if len(certificates) > 0 {
		now := time.Now()
		for _, x509Cert := range certificates {
			if !x509Cert.Expires().IsZero() && now.After(x509Cert.Expires()) {
				return nil, &rtcerr.InvalidAccessError{Err: ErrCertificateExpired}
			}
			t.certificates = append(t.certificates, x509Cert)
		}
	} else {
		sk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, &rtcerr.UnknownError{Err: err}
		}
		certificate, err := GenerateCertificate(sk)
		if err != nil {
			return nil, err
		}
		t.certificates = []Certificate{*certificate}
	}

	return t, nil
}

// ICETransport returns the currently-configured *ICETransport or nil
// if one has not been configured
func (t *DTLSTransport) ICETransport() *ICETransport {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.iceTransport
}

// onStateChange requires the caller holds the lock
func (t *DTLSTransport) onStateChange(state DTLSTransportState) {
	t.state = state
	hdlr := t.onStateChangeHdlr
	if hdlr != nil {
		hdlr(state)
	}
}

// OnStateChange sets a handler that is fired when the DTLS
// connection state changes.
func (t *DTLSTransport) OnStateChange(f func(DTLSTransportState)) {
	t.lock.Lock()
	defer t.lock.Unlock()
	t.onStateChangeHdlr = f
}

// State returns the current dtls transport state.
func (t *DTLSTransport) State() DTLSTransportState {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.state
}

// GetLocalParameters returns the DTLS parameters of the local DTLSTransport upon construction.
func (t *DTLSTransport) GetLocalParameters() (DTLSParameters, error) {
	fingerprints := []DTLSFingerprint{}

	for _, c := range t.certificates {
		prints, err := c.GetFingerprints()
		if err != nil {
			return DTLSParameters{}, err
		}

		fingerprints = append(fingerprints, prints...)
	}

	return DTLSParameters{
		Role:         DTLSRoleAuto, // always returns the default role
		Fingerprints: fingerprints,
	}, nil
}

// GetRemoteCertificate returns the certificate chain in use by the remote side
// returns an empty list prior to selection of the remote certificate
func (t *DTLSTransport) GetRemoteCertificate() []byte {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.remoteCertificate
}

func (t *DTLSTransport) startSRTP() error {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.srtpSession != nil && t.srtcpSession != nil {
		return nil
	} else if t.conn == nil {
		return fmt.Errorf("the DTLS transport has not started yet")
	}

	srtpConfig := &srtp.Config{
		Profile:       srtp.ProtectionProfileAes128CmHmacSha1_80,
		LoggerFactory: t.api.settingEngine.LoggerFactory,
	}
	if t.api.settingEngine.replayProtection.SRTP != nil {
		srtpConfig.RemoteOptions = append(
			srtpConfig.RemoteOptions,
			srtp.SRTPReplayProtection(*t.api.settingEngine.replayProtection.SRTP),
		)
	}

	if t.api.settingEngine.disableSRTPReplayProtection {
		srtpConfig.RemoteOptions = append(
			srtpConfig.RemoteOptions,
			srtp.SRTPNoReplayProtection(),
		)
	}

	if t.api.settingEngine.replayProtection.SRTCP != nil {
		srtpConfig.RemoteOptions = append(
			srtpConfig.RemoteOptions,
			srtp.SRTCPReplayProtection(*t.api.settingEngine.replayProtection.SRTCP),
		)
	}

	if t.api.settingEngine.disableSRTCPReplayProtection {
		srtpConfig.RemoteOptions = append(
			srtpConfig.RemoteOptions,
			srtp.SRTCPNoReplayProtection(),
		)
	}

	connState := t.conn.ConnectionState()
	err := srtpConfig.ExtractSessionKeysFromDTLS(&connState, t.role() == DTLSRoleClient)
	if err != nil {
		return fmt.Errorf("failed to extract sctp session keys: %v", err)
	}
	//SRTP session
	//传入的conn为rtp的Endpoint，说明将从Endpoint的buffer中读取数据
	srtpSession, err := srtp.NewSessionSRTP(t.srtpEndpoint, srtpConfig)
	if err != nil {
		return fmt.Errorf("failed to start srtp: %v", err)
	}
	//SRTCP session
	//传入的conn为rtcp的Endpoint，说明将从Endpoint的buffer中读取数据
	srtcpSession, err := srtp.NewSessionSRTCP(t.srtcpEndpoint, srtpConfig)
	if err != nil {
		return fmt.Errorf("failed to start srtp: %v", err)
	}

	t.srtpSession = srtpSession
	t.srtcpSession = srtcpSession
	return nil
}

func (t *DTLSTransport) getSRTPSession() (*srtp.SessionSRTP, error) {
	t.lock.RLock()
	if t.srtpSession != nil {
		t.lock.RUnlock()
		return t.srtpSession, nil
	}
	t.lock.RUnlock()

	if err := t.startSRTP(); err != nil {
		return nil, err
	}

	return t.srtpSession, nil
}

func (t *DTLSTransport) getSRTCPSession() (*srtp.SessionSRTCP, error) {
	t.lock.RLock()
	if t.srtcpSession != nil {
		t.lock.RUnlock()
		return t.srtcpSession, nil
	}
	t.lock.RUnlock()

	if err := t.startSRTP(); err != nil {
		return nil, err
	}

	return t.srtcpSession, nil
}

//todo:同时支持rtp/rtcp加密与不加密
func (t *DTLSTransport) getRTPSession() (*muxrtp.SessionRTP, error) {
	t.lock.RLock()
	if t.rtpSession != nil {
		t.lock.RUnlock()
		return t.rtpSession, nil
	}
	t.lock.RUnlock()
	var err error
	t.rtpSession, err = muxrtp.NewSessionRTP(t.rtpEndpoint)
	if err != nil {
		return nil, fmt.Errorf("muxrtp.NewSessionRTP => %s", err.Error())
	}
	t.rtcpSession, err = muxrtp.NewSessionRTCP(t.rtcpEndpoint)
	if err != nil {
		return nil, fmt.Errorf("muxrtp.NewSessionRTCP => %s", err.Error())
	}

	return t.rtpSession, nil
}

//todo:同时支持rtp/rtcp加密与不加密
func (t *DTLSTransport) getRTCPSession() (*muxrtp.SessionRTCP, error) {
	t.lock.RLock()
	if t.rtcpSession != nil {
		t.lock.RUnlock()
		return t.rtcpSession, nil
	}
	t.lock.RUnlock()

	var err error
	t.rtpSession, err = muxrtp.NewSessionRTP(t.rtpEndpoint)
	if err != nil {
		return nil, fmt.Errorf("muxrtp.NewSessionRTP => %s", err.Error())
	}
	t.rtcpSession, err = muxrtp.NewSessionRTCP(t.rtcpEndpoint)
	if err != nil {
		return nil, fmt.Errorf("muxrtp.NewSessionRTCP => %s", err.Error())
	}

	return t.rtcpSession, nil
}

func (t *DTLSTransport) role() DTLSRole {
	// If remote has an explicit role use the inverse
	switch t.remoteParameters.Role {
	case DTLSRoleClient:
		return DTLSRoleServer
	case DTLSRoleServer:
		return DTLSRoleClient
	}

	// If SettingEngine has an explicit role
	switch t.api.settingEngine.answeringDTLSRole {
	case DTLSRoleServer:
		return DTLSRoleServer
	case DTLSRoleClient:
		return DTLSRoleClient
	}

	// Remote was auto and no explicit role was configured via SettingEngine
	if t.iceTransport.Role() == ICERoleControlling {
		return DTLSRoleClient
	}
	return defaultDtlsRoleAnswer
}

// Start DTLS transport negotiation with the parameters of the remote DTLS transport
//todo:同时支持rtp/rtcp加密与不加密
func (t *DTLSTransport) Start(remoteParameters DTLSParameters) error {
	// Take lock and prepare connection, we must not hold the lock
	// when connecting
	prepareTransport := func() (DTLSRole, *dtls.Config, error) {
		t.lock.Lock()
		defer t.lock.Unlock()

		if err := t.ensureICEConn(); err != nil {
			return DTLSRole(0), nil, err
		}

		if t.state != DTLSTransportStateNew {
			return DTLSRole(0), nil, &rtcerr.InvalidStateError{Err: fmt.Errorf("attempted to start DTLSTransport that is not in new state: %s", t.state)}
		}
		//添加rtp/rtcp的Endpoint
		//在底层t.iceTransport.mux之上注册新的rtp/rtcp Endpoint，并添加对应Match function
		//TODO:重要!!! mux(多路复用)是联系 Endpoint 和 ICE的纽带
		//因为rtp与rtcp复用一个连接，所以为了区分rtp/rtcp包，为不同rtp/rtcp endPoint注册不同匹配函数，以匹配不同的数据包，存入不同的endPoint
		//todo: 为了避免影响其他模块需要dtls，此处保留dtls，只是使用不同的endpoint，虽然禁用dtls加密，但是dtls依然建立连接
		if remoteParameters.DisableEncrypt { //开启DTLS加密
			t.rtpEndpoint = t.iceTransport.NewEndpoint(mux.MatchRTP)
			t.rtcpEndpoint = t.iceTransport.NewEndpoint(mux.MatchRTCP)
		} else { //开启DTLS加密
			t.srtpEndpoint = t.iceTransport.NewEndpoint(mux.MatchSRTP)
			t.srtcpEndpoint = t.iceTransport.NewEndpoint(mux.MatchSRTCP)
		}
		t.remoteParameters = remoteParameters

		cert := t.certificates[0]
		t.onStateChange(DTLSTransportStateConnecting)

		return t.role(), &dtls.Config{
			Certificates: []tls.Certificate{
				{
					Certificate: [][]byte{cert.x509Cert.Raw},
					PrivateKey:  cert.privateKey,
				}},
			SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{dtls.SRTP_AES128_CM_HMAC_SHA1_80},
			ClientAuth:             dtls.RequireAnyClientCert,
			LoggerFactory:          t.api.settingEngine.LoggerFactory,
			InsecureSkipVerify:     true,
		}, nil
	}

	var dtlsConn *dtls.Conn
	//TODO: 获取 rtp/rtcp/dtls Endpoint
	//在底层t.iceTransport.mux之上注册新的DTLS Endpoint
	dtlsEndpoint := t.iceTransport.NewEndpoint(mux.MatchDTLS)
	//在底层t.iceTransport.mux之上注册新的rtp/rtcp Endpoint
	role, dtlsConfig, err := prepareTransport()
	if err != nil {
		return err
	}

	if t.api.settingEngine.replayProtection.DTLS != nil {
		dtlsConfig.ReplayProtectionWindow = int(*t.api.settingEngine.replayProtection.DTLS)
	}

	// Connect as DTLS Client/Server, function is blocking and we
	// must not hold the DTLSTransport lock
	/*
		DTLS连接，客户端和服务端交换密钥
		1、客户端发送ClientHello
		2、服务端回复HelloVerifyRequest
		3、客户端再次发送ClientHello
		4、服务端返回证书公钥
		5、客户端验证证书合法性，如果合法，则使用服务端的公钥加密密钥后发送给服务端
		6、服务端利用认证私钥解密，获得密钥
		7、客户端与服务端使用该密钥进行数据加密传输

	*/
	if role == DTLSRoleClient {
		dtlsConn, err = dtls.Client(dtlsEndpoint, dtlsConfig)
	} else {
		dtlsConn, err = dtls.Server(dtlsEndpoint, dtlsConfig)
	}

	// Re-take the lock, nothing beyond here is blocking
	t.lock.Lock()
	defer t.lock.Unlock()

	if err != nil {
		t.onStateChange(DTLSTransportStateFailed)
		return err
	}
	//保存dtls连接
	t.conn = dtlsConn
	t.onStateChange(DTLSTransportStateConnected)

	if t.api.settingEngine.disableCertificateFingerprintVerification {
		return nil
	}
	//获得对端交换的dtls certificate
	// Check the fingerprint if a certificate was exchanged
	remoteCerts := t.conn.ConnectionState().PeerCertificates
	if len(remoteCerts) == 0 {
		t.onStateChange(DTLSTransportStateFailed)
		return fmt.Errorf("peer didn't provide certificate via DTLS")
	}
	//存储对端发送的密钥
	t.remoteCertificate = remoteCerts[0]
	//parse密钥并检验其合法性
	parsedRemoteCert, err := x509.ParseCertificate(t.remoteCertificate)
	if err != nil {
		t.onStateChange(DTLSTransportStateFailed)
		return err
	}

	err = t.validateFingerPrint(parsedRemoteCert)
	if err != nil {
		t.onStateChange(DTLSTransportStateFailed)
	}
	return err
}

// Stop stops and closes the DTLSTransport object.
func (t *DTLSTransport) Stop() error {
	t.lock.Lock()
	defer t.lock.Unlock()

	// Try closing everything and collect the errors
	var closeErrs []error

	if t.srtpSession != nil {
		if err := t.srtpSession.Close(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}

	if t.srtcpSession != nil {
		if err := t.srtcpSession.Close(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}

	if t.conn != nil {
		// dtls connection may be closed on sctp close.
		if err := t.conn.Close(); err != nil && err != dtls.ErrConnClosed {
			closeErrs = append(closeErrs, err)
		}
	}
	t.onStateChange(DTLSTransportStateClosed)
	return util.FlattenErrs(closeErrs)
}

func (t *DTLSTransport) validateFingerPrint(remoteCert *x509.Certificate) error {
	for _, fp := range t.remoteParameters.Fingerprints {
		hashAlgo, err := fingerprint.HashFromString(fp.Algorithm)
		if err != nil {
			return err
		}

		remoteValue, err := fingerprint.Fingerprint(remoteCert, hashAlgo)
		if err != nil {
			return err
		}

		if strings.EqualFold(remoteValue, fp.Value) {
			return nil
		}
	}

	return errors.New("no matching fingerprint")
}

func (t *DTLSTransport) ensureICEConn() error {
	if t.iceTransport == nil || t.iceTransport.State() == ICETransportStateNew {
		return errors.New("ICE connection not started")
	}

	return nil
}
