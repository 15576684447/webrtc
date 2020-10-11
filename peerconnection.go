// +build !js

// Package webrtc implements the WebRTC 1.0 as defined in W3C WebRTC specification document.
package webrtc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	mathRand "math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
	"webrtc/webrtc/internal/util"
	"webrtc/webrtc/pkg/rtcerr"

	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v2"
)

// PeerConnection represents a WebRTC connection that establishes a
// peer-to-peer communications with another PeerConnection instance in a
// browser, or to another endpoint implementing the required protocols.
type PeerConnection struct {
	statsID string
	mu      sync.RWMutex

	// ops is an operations queue which will ensure the enqueued actions are
	// executed in order. It is used for asynchronously, but serially processing
	// remote and local descriptions
	ops *operations

	configuration Configuration

	currentLocalDescription  *SessionDescription
	pendingLocalDescription  *SessionDescription
	currentRemoteDescription *SessionDescription
	pendingRemoteDescription *SessionDescription
	signalingState           SignalingState
	iceConnectionState       ICEConnectionState
	connectionState          PeerConnectionState

	idpLoginURL *string

	isClosed                     *atomicBool
	negotiationNeeded            bool
	nonTrickleCandidatesSignaled *atomicBool

	lastOffer  string
	lastAnswer string

	// a value containing the last known greater mid value
	// we internally generate mids as numbers. Needed since JSEP
	// requires that when reusing a media section a new unique mid
	// should be defined (see JSEP 3.4.1).
	greaterMid int

	rtpTransceivers []*RTPTransceiver

	onSignalingStateChangeHandler     func(SignalingState)
	onICEConnectionStateChangeHandler func(ICEConnectionState)
	onConnectionStateChangeHandler    func(PeerConnectionState)
	onTrackHandler                    func(*Track, *RTPReceiver)
	onDataChannelHandler              func(*DataChannel)

	iceGatherer   *ICEGatherer
	iceTransport  *ICETransport
	dtlsTransport *DTLSTransport
	sctpTransport *SCTPTransport

	// A reference to the associated API state used by this connection
	api *API
	log logging.LeveledLogger
}

// NewPeerConnection creates a peerconnection with the default
// codecs. See API.NewRTCPeerConnection for details.
func NewPeerConnection(configuration Configuration) (*PeerConnection, error) {
	m := MediaEngine{}
	m.RegisterDefaultCodecs()
	api := NewAPI(WithMediaEngine(m))
	return api.NewPeerConnection(configuration)
}

// NewPeerConnection creates a new PeerConnection with the provided configuration against the received API object
func (api *API) NewPeerConnection(configuration Configuration) (*PeerConnection, error) {
	// https://w3c.github.io/webrtc-pc/#constructor (Step #2)
	// Some variables defined explicitly despite their implicit zero values to
	// allow better readability to understand what is happening.
	pc := &PeerConnection{
		statsID: fmt.Sprintf("PeerConnection-%d", time.Now().UnixNano()),
		ops:     newOperations(),
		configuration: Configuration{
			ICEServers:           []ICEServer{},
			ICETransportPolicy:   ICETransportPolicyAll,
			BundlePolicy:         BundlePolicyBalanced,
			RTCPMuxPolicy:        RTCPMuxPolicyRequire,
			Certificates:         []Certificate{},
			ICECandidatePoolSize: 0,
		},
		isClosed:                     &atomicBool{},
		negotiationNeeded:            false,
		nonTrickleCandidatesSignaled: &atomicBool{},
		lastOffer:                    "",
		lastAnswer:                   "",
		greaterMid:                   -1,
		signalingState:               SignalingStateStable,
		iceConnectionState:           ICEConnectionStateNew,
		connectionState:              PeerConnectionStateNew,

		api: api,
		log: api.settingEngine.LoggerFactory.NewLogger("pc"),
	}
	pc.log.Debugf("NewPeerConnection: before config: %+#v\n", pc)
	var err error
	//如果configuration指定了对应参数，就用以替换pc对应的默认值
	if err = pc.initConfiguration(configuration); err != nil {
		return nil, err
	}
	pc.log.Debugf("NewPeerConnection: after config: %+#v\n", pc)
	pc.log.Debugf("NewPeerConnection: start createICEGatherer\n")
	pc.iceGatherer, err = pc.createICEGatherer()
	if err != nil {
		return nil, err
	}
	pc.log.Debugf("NewPeerConnection: end createICEGatherer\n")
	pc.log.Debugf("NewPeerConnection: ICETrickle=%v\n", pc.api.settingEngine.candidates.ICETrickle)
	if !pc.api.settingEngine.candidates.ICETrickle {
		pc.log.Debugf("NewPeerConnection: pc.iceGatherer.Gather()\n")
		if err = pc.iceGatherer.Gather(); err != nil {
			return nil, err
		}
	}
	pc.log.Debugf("NewPeerConnection: createICETransport\n")
	// Create the ice transport
	iceTransport := pc.createICETransport()
	pc.iceTransport = iceTransport
	pc.log.Debugf("NewPeerConnection: NewDTLSTransport\n")
	// Create the DTLS transport
	dtlsTransport, err := pc.api.NewDTLSTransport(pc.iceTransport, pc.configuration.Certificates)
	if err != nil {
		return nil, err
	}
	pc.dtlsTransport = dtlsTransport
	pc.log.Debugf("NewPeerConnection: NewSCTPTransport\n")
	// Create the SCTP transport
	pc.sctpTransport = pc.api.NewSCTPTransport(pc.dtlsTransport)

	// Wire up the on datachannel handler
	pc.sctpTransport.OnDataChannel(func(d *DataChannel) {
		pc.mu.RLock()
		hdlr := pc.onDataChannelHandler
		pc.mu.RUnlock()
		if hdlr != nil {
			hdlr(d)
		}
	})

	return pc, nil
}

// initConfiguration defines validation of the specified Configuration and
// its assignment to the internal configuration variable. This function differs
// from its SetConfiguration counterpart because most of the checks do not
// include verification statements related to the existing state. Thus the
// function describes only minor verification of some the struct variables.
func (pc *PeerConnection) initConfiguration(configuration Configuration) error {
	if configuration.PeerIdentity != "" {
		pc.configuration.PeerIdentity = configuration.PeerIdentity
	}

	// https://www.w3.org/TR/webrtc/#constructor (step #3)
	if len(configuration.Certificates) > 0 {
		now := time.Now()
		for _, x509Cert := range configuration.Certificates {
			if !x509Cert.Expires().IsZero() && now.After(x509Cert.Expires()) {
				return &rtcerr.InvalidAccessError{Err: ErrCertificateExpired}
			}
			pc.configuration.Certificates = append(pc.configuration.Certificates, x509Cert)
		}
	} else {
		sk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return &rtcerr.UnknownError{Err: err}
		}
		certificate, err := GenerateCertificate(sk)
		if err != nil {
			return err
		}
		pc.configuration.Certificates = []Certificate{*certificate}
	}

	if configuration.BundlePolicy != BundlePolicy(Unknown) {
		pc.configuration.BundlePolicy = configuration.BundlePolicy
	}

	if configuration.RTCPMuxPolicy != RTCPMuxPolicy(Unknown) {
		pc.configuration.RTCPMuxPolicy = configuration.RTCPMuxPolicy
	}

	if configuration.ICECandidatePoolSize != 0 {
		pc.configuration.ICECandidatePoolSize = configuration.ICECandidatePoolSize
	}

	if configuration.ICETransportPolicy != ICETransportPolicy(Unknown) {
		pc.configuration.ICETransportPolicy = configuration.ICETransportPolicy
	}

	if configuration.SDPSemantics != SDPSemantics(Unknown) {
		pc.configuration.SDPSemantics = configuration.SDPSemantics
	}
	//获取正确的url
	sanitizedICEServers := configuration.getICEServers()
	if len(sanitizedICEServers) > 0 {
		for _, server := range sanitizedICEServers {
			if err := server.validate(); err != nil {
				return err
			}
		}
		pc.configuration.ICEServers = sanitizedICEServers
	}

	return nil
}

// OnSignalingStateChange sets an event handler which is invoked when the
// peer connection's signaling state changes
func (pc *PeerConnection) OnSignalingStateChange(f func(SignalingState)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onSignalingStateChangeHandler = f
}

func (pc *PeerConnection) onSignalingStateChange(newState SignalingState) {
	pc.mu.RLock()
	hdlr := pc.onSignalingStateChangeHandler
	pc.mu.RUnlock()

	pc.log.Infof("signaling state changed to %s", newState)
	if hdlr != nil {
		go hdlr(newState)
	}
}

// OnDataChannel sets an event handler which is invoked when a data
// channel message arrives from a remote peer.
func (pc *PeerConnection) OnDataChannel(f func(*DataChannel)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onDataChannelHandler = f
}

// OnICECandidate sets an event handler which is invoked when a new ICE
// candidate is found.
// Take note that the handler is gonna be called with a nil pointer when
// gathering is finished.
func (pc *PeerConnection) OnICECandidate(f func(*ICECandidate)) {
	pc.iceGatherer.OnLocalCandidate(f)
}

// OnICEGatheringStateChange sets an event handler which is invoked when the
// ICE candidate gathering state has changed.
func (pc *PeerConnection) OnICEGatheringStateChange(f func(ICEGathererState)) {
	pc.iceGatherer.OnStateChange(f)
}

// OnTrack sets an event handler which is called when remote track
// arrives from a remote peer.
func (pc *PeerConnection) OnTrack(f func(*Track, *RTPReceiver)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onTrackHandler = f
}

func (pc *PeerConnection) onTrack(t *Track, r *RTPReceiver) {
	pc.mu.RLock()
	hdlr := pc.onTrackHandler
	pc.mu.RUnlock()

	pc.log.Debugf("got new track: %+v", t)
	if hdlr != nil && t != nil {
		go hdlr(t, r)
	}
}

// OnICEConnectionStateChange sets an event handler which is called
// when an ICE connection state is changed.
func (pc *PeerConnection) OnICEConnectionStateChange(f func(ICEConnectionState)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onICEConnectionStateChangeHandler = f
}

func (pc *PeerConnection) onICEConnectionStateChange(cs ICEConnectionState) {
	pc.mu.Lock()
	pc.iceConnectionState = cs
	hdlr := pc.onICEConnectionStateChangeHandler
	pc.mu.Unlock()

	pc.log.Infof("ICE connection state changed: %s", cs)
	if hdlr != nil {
		go hdlr(cs)
	}
}

// OnConnectionStateChange sets an event handler which is called
// when the PeerConnectionState has changed
func (pc *PeerConnection) OnConnectionStateChange(f func(PeerConnectionState)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onConnectionStateChangeHandler = f
}

// SetConfiguration updates the configuration of this PeerConnection object.
func (pc *PeerConnection) SetConfiguration(configuration Configuration) error {
	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-setconfiguration (step #2)
	if pc.isClosed.get() {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #3)
	if configuration.PeerIdentity != "" {
		if configuration.PeerIdentity != pc.configuration.PeerIdentity {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingPeerIdentity}
		}
		pc.configuration.PeerIdentity = configuration.PeerIdentity
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #4)
	if len(configuration.Certificates) > 0 {
		if len(configuration.Certificates) != len(pc.configuration.Certificates) {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingCertificates}
		}

		for i, certificate := range configuration.Certificates {
			if !pc.configuration.Certificates[i].Equals(certificate) {
				return &rtcerr.InvalidModificationError{Err: ErrModifyingCertificates}
			}
		}
		pc.configuration.Certificates = configuration.Certificates
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #5)
	if configuration.BundlePolicy != BundlePolicy(Unknown) {
		if configuration.BundlePolicy != pc.configuration.BundlePolicy {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingBundlePolicy}
		}
		pc.configuration.BundlePolicy = configuration.BundlePolicy
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #6)
	if configuration.RTCPMuxPolicy != RTCPMuxPolicy(Unknown) {
		if configuration.RTCPMuxPolicy != pc.configuration.RTCPMuxPolicy {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingRTCPMuxPolicy}
		}
		pc.configuration.RTCPMuxPolicy = configuration.RTCPMuxPolicy
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #7)
	if configuration.ICECandidatePoolSize != 0 {
		if pc.configuration.ICECandidatePoolSize != configuration.ICECandidatePoolSize &&
			pc.LocalDescription() != nil {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingICECandidatePoolSize}
		}
		pc.configuration.ICECandidatePoolSize = configuration.ICECandidatePoolSize
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #8)
	if configuration.ICETransportPolicy != ICETransportPolicy(Unknown) {
		pc.configuration.ICETransportPolicy = configuration.ICETransportPolicy
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #11)
	if len(configuration.ICEServers) > 0 {
		// https://www.w3.org/TR/webrtc/#set-the-configuration (step #11.3)
		for _, server := range configuration.ICEServers {
			if err := server.validate(); err != nil {
				return err
			}
		}
		pc.configuration.ICEServers = configuration.ICEServers
	}
	return nil
}

// GetConfiguration returns a Configuration object representing the current
// configuration of this PeerConnection object. The returned object is a
// copy and direct mutation on it will not take affect until SetConfiguration
// has been called with Configuration passed as its only argument.
// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-getconfiguration
func (pc *PeerConnection) GetConfiguration() Configuration {
	return pc.configuration
}

func (pc *PeerConnection) getStatsID() string {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	return pc.statsID
}

// CreateOffer starts the PeerConnection and generates the localDescription
func (pc *PeerConnection) CreateOffer(options *OfferOptions) (SessionDescription, error) {
	useIdentity := pc.idpLoginURL != nil
	switch {
	case options != nil:
		return SessionDescription{}, fmt.Errorf("TODO handle options")
	case useIdentity:
		return SessionDescription{}, fmt.Errorf("TODO handle identity provider")
	case pc.isClosed.get():
		return SessionDescription{}, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	isPlanB := pc.configuration.SDPSemantics == SDPSemanticsPlanB
	if pc.currentRemoteDescription != nil {
		isPlanB = descriptionIsPlanB(pc.RemoteDescription())
	}

	// include unmatched local transceivers
	if !isPlanB {
		// update the greater mid if the remote description provides a greater one
		if pc.currentRemoteDescription != nil {
			for _, media := range pc.currentRemoteDescription.parsed.MediaDescriptions {
				mid := getMidValue(media)
				if mid == "" {
					continue
				}
				numericMid, err := strconv.Atoi(mid)
				if err != nil {
					continue
				}
				if numericMid > pc.greaterMid {
					pc.greaterMid = numericMid
				}
			}
		}
		for _, t := range pc.GetTransceivers() {
			if t.Mid() != "" {
				continue
			}
			pc.greaterMid++
			err := t.setMid(strconv.Itoa(pc.greaterMid))
			if err != nil {
				return SessionDescription{}, err
			}
		}
	}

	var (
		d   *sdp.SessionDescription
		err error
	)

	if pc.currentRemoteDescription == nil {
		d, err = pc.generateUnmatchedSDP(useIdentity)
	} else {
		d, err = pc.generateMatchedSDP(useIdentity, true /*includeUnmatched */, connectionRoleFromDtlsRole(defaultDtlsRoleOffer))
	}
	if err != nil {
		return SessionDescription{}, err
	}

	sdpBytes, err := d.Marshal()
	if err != nil {
		return SessionDescription{}, err
	}

	desc := SessionDescription{
		Type:   SDPTypeOffer,
		SDP:    string(sdpBytes),
		parsed: d,
	}
	pc.lastOffer = desc.SDP
	return desc, nil
}

func (pc *PeerConnection) createICEGatherer() (*ICEGatherer, error) {
	//指定IceServers以及Ice搜集策略
	pc.log.Debugf("createICEGatherer: IceService=%s, ICETransportPolicy: %s\n", pc.configuration.getICEServers(), pc.configuration.ICETransportPolicy)
	g, err := pc.api.NewICEGatherer(ICEGatherOptions{
		ICEServers:      pc.configuration.getICEServers(),
		ICEGatherPolicy: pc.configuration.ICETransportPolicy,
	})
	if err != nil {
		return nil, err
	}

	return g, nil
}

// Update the PeerConnectionState given the state of relevant transports
// https://www.w3.org/TR/webrtc/#rtcpeerconnectionstate-enum
func (pc *PeerConnection) updateConnectionState(iceConnectionState ICEConnectionState, dtlsTransportState DTLSTransportState) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	connectionState := PeerConnectionStateNew
	switch {
	// The RTCPeerConnection object's [[IsClosed]] slot is true.
	case pc.isClosed.get():
		connectionState = PeerConnectionStateClosed

	// Any of the RTCIceTransports or RTCDtlsTransports are in a "failed" state.
	case iceConnectionState == ICEConnectionStateFailed || dtlsTransportState == DTLSTransportStateFailed:
		connectionState = PeerConnectionStateFailed

	// Any of the RTCIceTransports or RTCDtlsTransports are in the "disconnected"
	// state and none of them are in the "failed" or "connecting" or "checking" state.  */
	case iceConnectionState == ICEConnectionStateDisconnected:
		connectionState = PeerConnectionStateDisconnected

	// All RTCIceTransports and RTCDtlsTransports are in the "connected", "completed" or "closed"
	// state and at least one of them is in the "connected" or "completed" state.
	case iceConnectionState == ICEConnectionStateConnected && dtlsTransportState == DTLSTransportStateConnected:
		connectionState = PeerConnectionStateConnected

	//  Any of the RTCIceTransports or RTCDtlsTransports are in the "connecting" or
	// "checking" state and none of them is in the "failed" state.
	case iceConnectionState == ICEConnectionStateChecking && dtlsTransportState == DTLSTransportStateConnecting:
		connectionState = PeerConnectionStateConnecting
	}

	if pc.connectionState == connectionState {
		return
	}

	pc.log.Infof("peer connection state changed: %s", connectionState)
	pc.connectionState = connectionState
	hdlr := pc.onConnectionStateChangeHandler
	if hdlr != nil {
		go hdlr(connectionState)
	}
}

func (pc *PeerConnection) createICETransport() *ICETransport {
	t := pc.api.NewICETransport(pc.iceGatherer)
	t.OnConnectionStateChange(func(state ICETransportState) {
		var cs ICEConnectionState
		switch state {
		case ICETransportStateNew:
			cs = ICEConnectionStateNew
		case ICETransportStateChecking:
			cs = ICEConnectionStateChecking
		case ICETransportStateConnected:
			cs = ICEConnectionStateConnected
		case ICETransportStateCompleted:
			cs = ICEConnectionStateCompleted
		case ICETransportStateFailed:
			cs = ICEConnectionStateFailed
		case ICETransportStateDisconnected:
			cs = ICEConnectionStateDisconnected
		case ICETransportStateClosed:
			cs = ICEConnectionStateClosed
		default:
			pc.log.Warnf("OnConnectionStateChange: unhandled ICE state: %s", state)
			return
		}
		pc.onICEConnectionStateChange(cs)
		pc.updateConnectionState(cs, pc.dtlsTransport.State())
	})

	return t
}

// CreateAnswer starts the PeerConnection and generates the localDescription
func (pc *PeerConnection) CreateAnswer(options *AnswerOptions) (SessionDescription, error) {
	useIdentity := pc.idpLoginURL != nil
	switch {
	case options != nil:
		return SessionDescription{}, fmt.Errorf("TODO handle options")
	case pc.RemoteDescription() == nil:
		return SessionDescription{}, &rtcerr.InvalidStateError{Err: ErrNoRemoteDescription}
	case useIdentity:
		return SessionDescription{}, fmt.Errorf("TODO handle identity provider")
	case pc.isClosed.get():
		return SessionDescription{}, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	connectionRole := connectionRoleFromDtlsRole(pc.api.settingEngine.answeringDTLSRole)
	if connectionRole == sdp.ConnectionRole(0) {
		connectionRole = connectionRoleFromDtlsRole(defaultDtlsRoleAnswer)
	}

	d, err := pc.generateMatchedSDP(useIdentity, false /*includeUnmatched */, connectionRole)
	if err != nil {
		return SessionDescription{}, err
	}

	sdpBytes, err := d.Marshal()
	if err != nil {
		return SessionDescription{}, err
	}

	desc := SessionDescription{
		Type:   SDPTypeAnswer,
		SDP:    string(sdpBytes),
		parsed: d,
	}
	pc.lastAnswer = desc.SDP
	return desc, nil
}

// 4.4.1.6 Set the SessionDescription
//详细state相关注释见checkNextSignalingState函数
func (pc *PeerConnection) setDescription(sd *SessionDescription, op stateChangeOp) error {
	if pc.isClosed.get() {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	nextState, err := func() (SignalingState, error) {
		pc.mu.Lock()
		defer pc.mu.Unlock()

		cur := pc.signalingState
		setLocal := stateChangeOpSetLocal
		setRemote := stateChangeOpSetRemote
		newSDPDoesNotMatchOffer := &rtcerr.InvalidModificationError{Err: fmt.Errorf("new sdp does not match previous offer")}
		newSDPDoesNotMatchAnswer := &rtcerr.InvalidModificationError{Err: fmt.Errorf("new sdp does not match previous answer")}

		var nextState SignalingState
		var err error
		//所有注释为: 当前状态 -> 将要执行的操作 -> 执行完操作后的状态
		//如果执行结束后状态为stable，则将该sd设置到currentRemoteDescription/currentLocalDescription
		//否则都设置到pendingRemoteDescription/pendingLocalDescription
		//一旦执行rollback回滚，currentRemoteDescription/currentLocalDescription都回到nil
		//即同时搜集完offer和answer才属于stable状态，否则都属于pending状态
		//pending状态包括只搜集到offer，或者offer+pranswer
		switch op {
		case setLocal:
			switch sd.Type {
			// stable->SetLocal(offer)->have-local-offer
			case SDPTypeOffer:
				if sd.SDP != pc.lastOffer {
					return nextState, newSDPDoesNotMatchOffer
				}
				nextState, err = checkNextSignalingState(cur, SignalingStateHaveLocalOffer, setLocal, sd.Type)
				if err == nil {
					pc.pendingLocalDescription = sd
				}
			// have-remote-offer->SetLocal(answer)->stable
			// have-local-pranswer->SetLocal(answer)->stable
			case SDPTypeAnswer:
				if sd.SDP != pc.lastAnswer {
					return nextState, newSDPDoesNotMatchAnswer
				}
				nextState, err = checkNextSignalingState(cur, SignalingStateStable, setLocal, sd.Type)
				if err == nil {
					pc.currentLocalDescription = sd
					pc.currentRemoteDescription = pc.pendingRemoteDescription
					pc.pendingRemoteDescription = nil
					pc.pendingLocalDescription = nil
				}
			case SDPTypeRollback:
				nextState, err = checkNextSignalingState(cur, SignalingStateStable, setLocal, sd.Type)
				if err == nil {
					pc.pendingLocalDescription = nil
				}
			// have-remote-offer->SetLocal(pranswer)->have-local-pranswer
			case SDPTypePranswer:
				if sd.SDP != pc.lastAnswer {
					return nextState, newSDPDoesNotMatchAnswer
				}
				nextState, err = checkNextSignalingState(cur, SignalingStateHaveLocalPranswer, setLocal, sd.Type)
				if err == nil {
					pc.pendingLocalDescription = sd
				}
			default:
				return nextState, &rtcerr.OperationError{Err: fmt.Errorf("invalid state change op: %s(%s)", op, sd.Type)}
			}
		case setRemote:
			switch sd.Type {
			// stable->SetRemote(offer)->have-remote-offer
			case SDPTypeOffer:
				nextState, err = checkNextSignalingState(cur, SignalingStateHaveRemoteOffer, setRemote, sd.Type)
				if err == nil {
					pc.pendingRemoteDescription = sd
				}
			// have-local-offer->SetRemote(answer)->stable
			// have-remote-pranswer->SetRemote(answer)->stable
			case SDPTypeAnswer:
				nextState, err = checkNextSignalingState(cur, SignalingStateStable, setRemote, sd.Type)
				if err == nil {
					pc.currentRemoteDescription = sd
					pc.currentLocalDescription = pc.pendingLocalDescription
					pc.pendingRemoteDescription = nil
					pc.pendingLocalDescription = nil
				}
			case SDPTypeRollback:
				nextState, err = checkNextSignalingState(cur, SignalingStateStable, setRemote, sd.Type)
				if err == nil {
					pc.pendingRemoteDescription = nil
				}
			// have-local-offer->SetRemote(pranswer)->have-remote-pranswer
			case SDPTypePranswer:
				nextState, err = checkNextSignalingState(cur, SignalingStateHaveRemotePranswer, setRemote, sd.Type)
				if err == nil {
					pc.pendingRemoteDescription = sd
				}
			default:
				return nextState, &rtcerr.OperationError{Err: fmt.Errorf("invalid state change op: %s(%s)", op, sd.Type)}
			}
		default:
			return nextState, &rtcerr.OperationError{Err: fmt.Errorf("unhandled state change op: %q", op)}
		}

		return nextState, nil
	}()

	if err == nil {
		pc.signalingState = nextState
		pc.onSignalingStateChange(nextState)
	}
	return err
}

// SetLocalDescription sets the SessionDescription of the local peer
func (pc *PeerConnection) SetLocalDescription(desc SessionDescription) error {
	if pc.isClosed.get() {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	haveLocalDescription := pc.currentLocalDescription != nil

	// JSEP 5.4
	if desc.SDP == "" {
		switch desc.Type {
		case SDPTypeAnswer, SDPTypePranswer:
			desc.SDP = pc.lastAnswer
		case SDPTypeOffer:
			desc.SDP = pc.lastOffer
		default:
			return &rtcerr.InvalidModificationError{
				Err: fmt.Errorf("invalid SDP type supplied to SetLocalDescription(): %s", desc.Type),
			}
		}
	}

	desc.parsed = &sdp.SessionDescription{}
	if err := desc.parsed.Unmarshal([]byte(desc.SDP)); err != nil {
		return err
	}
	if err := pc.setDescription(&desc, stateChangeOpSetLocal); err != nil {
		return err
	}

	weAnswer := desc.Type == SDPTypeAnswer
	remoteDesc := pc.RemoteDescription()
	if weAnswer && remoteDesc != nil {
		pc.ops.Enqueue(func() {
			pc.startRTP(haveLocalDescription, remoteDesc)
		})
	}

	// To support all unittests which are following the future trickle=true
	// setup while also support the old trickle=false synchronous gathering
	// process this is necessary to avoid calling Gather() in multiple
	// places; which causes race conditions. (issue-707)
	if !pc.api.settingEngine.candidates.ICETrickle && !pc.nonTrickleCandidatesSignaled.get() {
		if err := pc.iceGatherer.SignalCandidates(); err != nil {
			return err
		}
		pc.nonTrickleCandidatesSignaled.set(true)
		return nil
	}

	if pc.iceGatherer.State() == ICEGathererStateNew {
		return pc.iceGatherer.Gather()
	}
	return nil
}

// LocalDescription returns PendingLocalDescription if it is not null and
// otherwise it returns CurrentLocalDescription. This property is used to
// determine if SetLocalDescription has already been called.
// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-localdescription
func (pc *PeerConnection) LocalDescription() *SessionDescription {
	if pendingLocalDescription := pc.PendingLocalDescription(); pendingLocalDescription != nil {
		return pendingLocalDescription
	}
	return pc.CurrentLocalDescription()
}

// SetRemoteDescription sets the SessionDescription of the remote peer
func (pc *PeerConnection) SetRemoteDescription(desc SessionDescription) error {
	if pc.isClosed.get() {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}
	//这里的意思应该是，如果已有RemoteDescription，则说明之前就调用过该函数，此时只需要重协商，而不需要重新建立ICE
	//如果没有RemoteDescription，则说明第一次调用，需要从头开始搜集candidate，探测candidate pair等
	haveRemoteDescription := pc.currentRemoteDescription != nil

	desc.parsed = &sdp.SessionDescription{}
	if err := desc.parsed.Unmarshal([]byte(desc.SDP)); err != nil {
		return err
	}
	pc.log.Debugf("SetRemoteDescription: setDescription state\n")
	//根据当前状态和将要执行的动作，决定下一个状态是否为stable，从而决定该session description是存储到pending(Remote/Local)Description还是正式current(Remote/Local)Description
	if err := pc.setDescription(&desc, stateChangeOpSetRemote); err != nil {
		return err
	}

	weOffer := desc.Type == SDPTypeAnswer

	var t *RTPTransceiver
	localTransceivers := append([]*RTPTransceiver{}, pc.GetTransceivers()...)
	detectedPlanB := descriptionIsPlanB(pc.RemoteDescription())

	/*
		TODO:RTPTransceiver意义及其与track的关系
			1、每个RTPTransceiver对应sdp中的一个<m=...>标签
			2、对于Plan B模式，本地要发送的多个相同媒体类型的local track（a=ssrc不同）可能会属于同一个mLine，
				因此，RTPTransceiver包含多个RtpSender的向量 ，每个RtpSender会存储其中一个local track。
			3、对于Unified Plan模式，RTPTransceiver的RtpSender向量实质上只会存在一个RtpSender。
			4、RTPTransceiver还包含一个RtpReceiver，用于保存remote track。
	*/
	//确定是否存在对应可用的transceiver，如果不存在，则新建一个
	if !weOffer && !detectedPlanB {
		//每个<m=...>对应一个transceiver
		for _, media := range pc.RemoteDescription().parsed.MediaDescriptions {
			//获取m标签对应的媒体类型audio/video/application
			midValue := getMidValue(media)
			if midValue == "" {
				return fmt.Errorf("RemoteDescription contained media section without mid value")
			}
			//application不使用transceiver
			if media.MediaName.Media == mediaSectionApplication {
				continue
			}
			//获取<m=...>对应的codec类型和direction
			kind := NewRTPCodecType(media.MediaName.Media)
			direction := getPeerDirection(media)
			if kind == 0 || direction == RTPTransceiverDirection(Unknown) {
				continue
			}
			//根据midValue匹配
			/*
				TODO:
					对于每个<m=...>，对应一个transceiver，首先从已有的transceiver中查找是否有可复用的
					可复用的条件为:
						1、transceiver类型与<m=...>类型一致，如都为audio/video
						2、transceiver中Sender的track为nil，即未被其他track占用
						3、transceiver的direction未被设置为send，即如果添加发送track，就需要设置direction为可发送
						4、transceiver状态不为stop
			*/
			t, localTransceivers = findByMid(midValue, localTransceivers)
			if t == nil {
				//进一步再根据RTPCodecType(audio/video)和传输方向匹配
				t, localTransceivers = satisfyTypeAndDirection(kind, direction, localTransceivers)
			}
			//如果未匹配到任何transceiver，则新建一个，此时只添加RTPReceiver，RTPSender置为nil
			if t == nil {
				receiver, err := pc.api.NewRTPReceiver(kind, pc.dtlsTransport)
				if err != nil {
					return err
				}
				t = pc.newRTPTransceiver(receiver, nil, RTPTransceiverDirectionRecvonly, kind)
			}
			if t.Mid() == "" {
				_ = t.setMid(midValue)
			}
		}
	}
	//如果已经有RemoteDescription并且我方Offer已经完毕，说明双方candidate搜集完毕，开始打洞
	if haveRemoteDescription {
		if weOffer {
			pc.ops.Enqueue(func() {
				pc.startRTP(true, &desc)
			})
		}
		return nil
	}
	//ICE模式分为 FULL ICE和Lite ICE
	//FULL ICE:双方都要进行连通性检查，完整地走一遍流程
	//Lite ICE: 在FULL ICE和Lite ICE互通时，只需要FULL ICE一方进行连通性检查，Lite一方只需回应response消息。这种模式对于部署在公网的设备比较常用。
	remoteIsLite := false
	//根据remoteSDP，判断remote的ICE模式是否为Lite模式
	if liteValue, haveRemoteIs := desc.parsed.Attribute(sdp.AttrKeyICELite); haveRemoteIs && liteValue == sdp.AttrKeyICELite {
		remoteIsLite = true
	}

	fingerprint, fingerprintHash, err := extractFingerprint(desc.parsed)
	if err != nil {
		return err
	}

	remoteUfrag, remotePwd, candidates, err := extractICEDetails(desc.parsed)
	if err != nil {
		return err
	}
	pc.log.Debugf("SetRemoteDescription: extract fingerprint=%s && (Ufrag=%s, Pwd=%s) from remote sdp\n", fingerprint, remoteUfrag, remotePwd)
	//遍历remoteSDP中的candidates,将remoteCandidate与同网络类型的LocalCandidate组成pair，进行连通性测试
	pc.log.Debugf("SetRemoteDescription: AddRemoteCandidate for candidates -> %+v\n", candidates)
	for _, c := range candidates {
		if err = pc.iceTransport.AddRemoteCandidate(c); err != nil {
			return err
		}
	}
	//ICE角色分为controlling和controlled，controlling为
	//如果其中一方ICE为Lite模式，另一方是Full模式，则Full方为controlling agent角色
	//如果双方是相同的角色，Offer一方为controlling角色，answer一方为controlled角色
	iceRole := ICERoleControlled
	// If one of the agents is lite and the other one is not, the lite agent must be the controlling agent.
	// If both or neither agents are lite the offering agent is controlling.
	// RFC 8445 S6.1.1
	if (weOffer && remoteIsLite == pc.api.settingEngine.candidates.ICELite) || (remoteIsLite && !pc.api.settingEngine.candidates.ICELite) {
		iceRole = ICERoleControlling
	}
	pc.log.Debugf("*************************SetRemoteDescription: weOffer=%v, remoteIsLite=%v, pc.api.settingEngine.candidates.ICELite=%v, iceRole=%s\n", weOffer, remoteIsLite, pc.api.settingEngine.candidates.ICELite, iceRole.String())
	// Start the networking in a new routine since it will block until
	// the connection is actually established.
	pc.log.Debugf("SetRemoteDescription: startTransports\n")
	pc.ops.Enqueue(func() {
		//进行连通性测试，需要重点阅读解析!!!
		pc.startTransports(iceRole, dtlsRoleFromRemoteSDP(desc.parsed), remoteUfrag, remotePwd, fingerprint, fingerprintHash)
		if weOffer {
			pc.startRTP(false, &desc)
		}
	})
	return nil
}

func (pc *PeerConnection) startReceiver(incoming trackDetails, receiver *RTPReceiver) {
	//获取RTPReceiver的rtpReadStream/rtcpReadStream
	//最终只需要读取rtpReadStream/rtcpReadStream的buffer即可
	/*
		TODO:疑问???: ICE探测可用的selectPair如何与rtp/rtcp的conn联系
			解释: selectPair上层封装了mux，即多路复用；与此同时，rtp/rtcp/dtls等构建了对应的EndPoint，底层使用了mux
			mux收到数据包后，根据EndPoint的matchFun，匹配到对应的EndPoint，将数据存放到对应EndPoint的buffer中，并发送notify到session
			rtc/rtcp session收到EndPoint的notify后，从EndPoint buffer中读取数据，并存储到对应ssrc的rtc/rtcp Stream对象的buffer中
			而一个rtp/rtcp Stream对应一个ssrc，而ssrc与track一一对应
			所以读取track数据就等同于从对应ssrc的rtp Stream的buffer中读取数据，session将数据写入对应ssrc的stream后就会通知对应ssrc的track来读取
			session与stream的对应关系为: 一个rtp/rtcp session可对应多个rtp/rtcp stream(如果存在多个ssrc)
			session只是一个中间产物，stream才是最终操作的句柄对象，session将数据写入stream后会通过chan通知对应track来读取数据
	*/
	/*
		TODO:Sender是在何时使用的
			要在某个pc上发送某个track，只要将该track添加到该pc即可
			在pc上添加track的过程，就是将pc上对应Transceiver的RTPSender添加到该track的activeSenders的过程
			最后当track收到数据后，就会遍历其activeSenders数组，并依次发送track数据到各个RTPSender
	*/
	err := receiver.Receive(RTPReceiveParameters{
		DisableEncrypt: pc.api.settingEngine.disableEncrypt,
		Encodings: RTPDecodingParameters{
			RTPCodingParameters{SSRC: incoming.ssrc},
		}})
	if err != nil {
		pc.log.Warnf("RTPReceiver Receive failed %s", err)
		return
	}

	// set track id and label early so they can be set as new track information
	// is received from the SDP.
	//设置track的track_id和stream_id
	receiver.Track().mu.Lock()
	receiver.Track().id = incoming.id
	receiver.Track().label = incoming.label
	receiver.Track().mu.Unlock()

	//todo: 此函数只会触发一次，在该track有数据到来时，触发一次onTrack，并不会重复触发，所以在OnTrack函数中需要调用for循环来持续读取track
	go func() {
		//从buffer中读取一个pkt来决定其payload类型
		if err = receiver.Track().determinePayloadType(); err != nil {
			pc.log.Warnf("Could not determine PayloadType for SSRC %d", receiver.Track().SSRC())
			return
		}

		pc.mu.RLock()
		defer pc.mu.RUnlock()
		//获取解码器
		codec, err := pc.api.mediaEngine.getCodec(receiver.Track().PayloadType())
		if err != nil {
			pc.log.Warnf("no codec could be found for payloadType %d", receiver.Track().PayloadType())
			return
		}
		//添加track的解码器信息
		receiver.Track().mu.Lock()
		receiver.Track().kind = codec.Type
		receiver.Track().codec = codec
		receiver.Track().mu.Unlock()
		//如果注册了onTrackHandler，则handle该track
		if pc.onTrackHandler != nil {
			pc.onTrack(receiver.Track(), receiver)
		} else {
			pc.log.Warnf("OnTrack unset, unable to handle incoming media streams")
		}
	}()
}

// startRTPReceivers opens knows inbound SRTP streams from the RemoteDescription
func (pc *PeerConnection) startRTPReceivers(incomingTracks map[uint32]trackDetails, currentTransceivers []*RTPTransceiver) {
	localTransceivers := append([]*RTPTransceiver{}, currentTransceivers...)
	//判断是SDP格式是PlanB还是UnifiedPlan模式
	remoteIsPlanB := false
	switch pc.configuration.SDPSemantics {
	case SDPSemanticsPlanB:
		remoteIsPlanB = true
	case SDPSemanticsUnifiedPlanWithFallback:
		//PlanB所有audio track共用一个m=audio，所有video track共用一个m=video，就是说每个m标签包含一个或多个同类track
		//UnifiedPlan每个track对应一个m标签
		//目前是从PlanB过渡到UnifiedPlan
		remoteIsPlanB = descriptionIsPlanB(pc.RemoteDescription())
	}

	// Ensure we haven't already started a transceiver for this ssrc
	//从incomingTracks中移除已经添加到localTransceivers的tracks，剩下还未添加到localTransceivers的tracks将在之后添加
	for ssrc := range incomingTracks {
		for i := range localTransceivers {
			if t := localTransceivers[i]; (t.Receiver()) == nil || t.Receiver().Track() == nil || t.Receiver().Track().ssrc != ssrc {
				continue
			}
			//如果localTransceivers已经存在了对应ssrc的Receiver，则从incomingTracks移除对应的ssrc，不再重复添加
			delete(incomingTracks, ssrc)
		}
	}
	//剩余incomingTracks中的ssrc都是未开启transceiver的
	//
	/*
		(type一致)查找transceiver中mid(媒体类型/audio/video)、kind(解码器类型)与incomingTracks中ssrc一致的、
		(可用于接收的Transceivers)direction为RTPTransceiverDirectionRecvonly或RTPTransceiverDirectionSendrecv的、
		(可用并且还没被其他track占用)Receiver不为nil并且还未收到过媒体数据的(即没有用于其他track的Receiver)
	*/
	//在peerConnection中开始该ssrc的接收器(pc.startReceiver)
	for ssrc, incoming := range incomingTracks {
		for i := range localTransceivers {
			t := localTransceivers[i]

			if t.Mid() != incoming.mid {
				continue
			}
			if (incomingTracks[ssrc].kind != t.kind) ||
				(t.Direction() != RTPTransceiverDirectionRecvonly && t.Direction() != RTPTransceiverDirectionSendrecv) ||
				(t.Receiver()) == nil ||
				(t.Receiver().haveReceived()) {
				continue
			}
			delete(incomingTracks, ssrc)
			localTransceivers = append(localTransceivers[:i], localTransceivers[i+1:]...)
			pc.startReceiver(incoming, t.Receiver())
			break
		}
	}

	if remoteIsPlanB {
		for ssrc, incoming := range incomingTracks {
			t, err := pc.AddTransceiverFromKind(incoming.kind, RtpTransceiverInit{
				Direction: RTPTransceiverDirectionSendrecv,
			})
			if err != nil {
				pc.log.Warnf("Could not add transceiver for remote SSRC %d: %s", ssrc, err)
				continue
			}
			pc.startReceiver(incoming, t.Receiver())
		}
	}
}

// startRTPSenders starts all outbound RTP streams
/*
TODO:这里有一个特别细微但是关键的细节
	startRTPSenders -> transceiver.Sender().Send -> r.track.activeSenders = append(r.track.activeSenders, r)
	同一个track添加到不同pc的RTPSender中，其实是指向了该track的地址，所以引用的同一个
	所以上述的结果就是将Sender都存放到了同一个track的activeSenders
	!!!这里实现的核心功能为: 将指向相同track的所有RTPSender放到该track的activeSenders中
	那么该track在发送数据时，只需要遍历其下的所有activeSenders，然后逐个发送即可
*/
func (pc *PeerConnection) startRTPSenders(currentTransceivers []*RTPTransceiver) {
	for _, transceiver := range currentTransceivers {
		// TODO(sgotti) when in future we'll avoid replacing a transceiver sender just check the transceiver negotiation status
		if transceiver.Sender() != nil && transceiver.Sender().isNegotiated() && !transceiver.Sender().hasSent() {
			err := transceiver.Sender().Send(RTPSendParameters{
				DisableEncrypt: pc.api.settingEngine.disableEncrypt,
				Encodings: RTPEncodingParameters{
					RTPCodingParameters{
						SSRC:        transceiver.Sender().track.SSRC(),
						PayloadType: transceiver.Sender().track.PayloadType(),
					},
				}})
			if err != nil {
				pc.log.Warnf("Failed to start Sender: %s", err)
			}
		}
	}
}

// Start SCTP subsystem
func (pc *PeerConnection) startSCTP() {
	// Start sctp
	if err := pc.sctpTransport.Start(SCTPCapabilities{
		MaxMessageSize: 0,
	}); err != nil {
		pc.log.Warnf("Failed to start SCTP: %s", err)
		if err = pc.sctpTransport.Stop(); err != nil {
			pc.log.Warnf("Failed to stop SCTPTransport: %s", err)
		}

		return
	}

	// DataChannels that need to be opened now that SCTP is available
	// make a copy we may have incoming DataChannels mutating this while we open
	pc.sctpTransport.lock.RLock()
	dataChannels := append([]*DataChannel{}, pc.sctpTransport.dataChannels...)
	pc.sctpTransport.lock.RUnlock()

	var openedDCCount uint32
	for _, d := range dataChannels {
		if d.ReadyState() == DataChannelStateConnecting {
			err := d.open(pc.sctpTransport)
			if err != nil {
				pc.log.Warnf("failed to open data channel: %s", err)
				continue
			}
			openedDCCount++
		}
	}

	pc.sctpTransport.lock.Lock()
	pc.sctpTransport.dataChannelsOpened += openedDCCount
	pc.sctpTransport.lock.Unlock()
}

// drainSRTP pulls and discards RTP/RTCP packets that don't match any a:ssrc lines
// If the remote SDP was only one media section the ssrc doesn't have to be explicitly declared
//todo: 拉取并丢弃那些不匹配的ssrc媒体数据。如果sdp中只有一项media section，则无需明确申明
func (pc *PeerConnection) drainSRTP() {
	handleUndeclaredSSRC := func(ssrc uint32) bool {
		if remoteDescription := pc.RemoteDescription(); remoteDescription != nil {
			if len(remoteDescription.parsed.MediaDescriptions) == 1 {
				onlyMediaSection := remoteDescription.parsed.MediaDescriptions[0]
				//sdp只有一项media section，无需明确申明ssrc项
				for _, a := range onlyMediaSection.Attributes {
					if a.Key == ssrcStr {
						return false
					}
				}
				//构造track与transceiver，并开启接收UndeclaredSSR
				incoming := trackDetails{
					ssrc: ssrc,
					kind: RTPCodecTypeVideo,
				}
				if onlyMediaSection.MediaName.Media == RTPCodecTypeAudio.String() {
					incoming.kind = RTPCodecTypeAudio
				}

				t, err := pc.AddTransceiverFromKind(incoming.kind, RtpTransceiverInit{
					Direction: RTPTransceiverDirectionSendrecv,
				})
				if err != nil {
					pc.log.Warnf("Could not add transceiver for remote SSRC %d: %s", ssrc, err)
					return false
				}
				pc.startReceiver(incoming, t.Receiver())
				return true
			}
		}

		return false
	}

	go func() {
		for {
			srtpSession, err := pc.dtlsTransport.getSRTPSession()
			if err != nil {
				pc.log.Warnf("drainSRTP failed to open SrtpSession: %v", err)
				return
			}
			//每次收到新的 undeclared ssrc则返回一次，否则一直阻塞
			//todo:如果是sdp中已有的ssrc，则在startReceiver时，就会构建Stream；只有不在sdp中申明的ssrc到来时，才会在此触发，并执行 handleUndeclaredSSRC，丢弃其rtp/rtcp包
			_, ssrc, err := srtpSession.AcceptStream()
			if err != nil {
				pc.log.Warnf("Failed to accept RTP %v", err)
				return
			}

			if !handleUndeclaredSSRC(ssrc) {
				pc.log.Warnf("Incoming unhandled RTP ssrc(%d), OnTrack will not be fired", ssrc)
			}
		}
	}()

	go func() {
		for {
			srtcpSession, err := pc.dtlsTransport.getSRTCPSession()
			if err != nil {
				pc.log.Warnf("drainSRTP failed to open SrtcpSession: %v", err)
				return
			}
			//每次收到新的 undeclared ssrc则返回一次，否则一直阻塞
			//todo:如果是sdp中已有的ssrc，则在startReceiver时，就会构建Stream；只有不在sdp中申明的ssrc到来时，才会在此触发，并执行 handleUndeclaredSSRC，丢弃其rtp/rtcp包
			_, ssrc, err := srtcpSession.AcceptStream()
			if err != nil {
				pc.log.Warnf("Failed to accept RTCP %v", err)
				return
			}
			pc.log.Warnf("Incoming unhandled RTCP ssrc(%d), OnTrack will not be fired", ssrc)
		}
	}()
}

func (pc *PeerConnection) drainRTP() {
	handleUndeclaredSSRC := func(ssrc uint32) bool {
		if remoteDescription := pc.RemoteDescription(); remoteDescription != nil {
			if len(remoteDescription.parsed.MediaDescriptions) == 1 {
				onlyMediaSection := remoteDescription.parsed.MediaDescriptions[0]
				//sdp只有一项media section，无需明确申明ssrc项
				for _, a := range onlyMediaSection.Attributes {
					if a.Key == ssrcStr {
						return false
					}
				}
				//构造track与transceiver，并开启接收UndeclaredSSR
				incoming := trackDetails{
					ssrc: ssrc,
					kind: RTPCodecTypeVideo,
				}
				if onlyMediaSection.MediaName.Media == RTPCodecTypeAudio.String() {
					incoming.kind = RTPCodecTypeAudio
				}

				t, err := pc.AddTransceiverFromKind(incoming.kind, RtpTransceiverInit{
					Direction: RTPTransceiverDirectionSendrecv,
				})
				if err != nil {
					pc.log.Warnf("Could not add transceiver for remote SSRC %d: %s", ssrc, err)
					return false
				}
				pc.startReceiver(incoming, t.Receiver())
				return true
			}
		}

		return false
	}

	go func() {
		for {
			rtpSession, err := pc.dtlsTransport.getRTPSession()
			if err != nil {
				pc.log.Warnf("drainRTP failed to open rtpSession: %v", err)
				return
			}
			//每次收到新的 undeclared ssrc则返回一次，否则一直阻塞
			//todo:如果是sdp中已有的ssrc，则在startReceiver时，就会构建Stream；只有不在sdp中申明的ssrc到来时，才会在此触发，并执行 handleUndeclaredSSRC，丢弃其rtp/rtcp包
			_, ssrc, err := rtpSession.AcceptStream()
			if err != nil {
				pc.log.Warnf("Failed to accept RTP %v", err)
				return
			}

			if !handleUndeclaredSSRC(ssrc) {
				pc.log.Warnf("Incoming unhandled RTP ssrc(%d), OnTrack will not be fired", ssrc)
			}
		}
	}()

	go func() {
		for {
			rtcpSession, err := pc.dtlsTransport.getRTCPSession()
			if err != nil {
				pc.log.Warnf("drainRTP failed to open rtcpSession: %v", err)
				return
			}
			//每次收到新的 undeclared ssrc则返回一次，否则一直阻塞
			//todo:如果是sdp中已有的ssrc，则在startReceiver时，就会构建Stream；只有不在sdp中申明的ssrc到来时，才会在此触发，并执行 handleUndeclaredSSRC，丢弃其rtp/rtcp包
			_, ssrc, err := rtcpSession.AcceptStream()
			if err != nil {
				pc.log.Warnf("Failed to accept RTCP %v", err)
				return
			}
			pc.log.Warnf("Incoming unhandled RTCP ssrc(%d), OnTrack will not be fired", ssrc)
		}
	}()
}

// RemoteDescription returns pendingRemoteDescription if it is not null and
// otherwise it returns currentRemoteDescription. This property is used to
// determine if setRemoteDescription has already been called.
// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-remotedescription
func (pc *PeerConnection) RemoteDescription() *SessionDescription {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	if pc.pendingRemoteDescription != nil {
		return pc.pendingRemoteDescription
	}
	return pc.currentRemoteDescription
}

// AddICECandidate accepts an ICE candidate string and adds it
// to the existing set of candidates
func (pc *PeerConnection) AddICECandidate(candidate ICECandidateInit) error {
	if pc.RemoteDescription() == nil {
		return &rtcerr.InvalidStateError{Err: ErrNoRemoteDescription}
	}

	candidateValue := strings.TrimPrefix(candidate.Candidate, "candidate:")
	attribute := sdp.NewAttribute("candidate", candidateValue)
	sdpCandidate, err := attribute.ToICECandidate()
	if err != nil {
		return err
	}

	iceCandidate, err := newICECandidateFromSDP(sdpCandidate)
	if err != nil {
		return err
	}

	return pc.iceTransport.AddRemoteCandidate(iceCandidate)
}

// ICEConnectionState returns the ICE connection state of the
// PeerConnection instance.
func (pc *PeerConnection) ICEConnectionState() ICEConnectionState {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	return pc.iceConnectionState
}

// GetSenders returns the RTPSender that are currently attached to this PeerConnection
func (pc *PeerConnection) GetSenders() []*RTPSender {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	result := []*RTPSender{}
	for _, transceiver := range pc.rtpTransceivers {
		if transceiver.Sender() != nil {
			result = append(result, transceiver.Sender())
		}
	}
	return result
}

// GetReceivers returns the RTPReceivers that are currently attached to this RTCPeerConnection
func (pc *PeerConnection) GetReceivers() []*RTPReceiver {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	result := []*RTPReceiver{}
	for _, transceiver := range pc.rtpTransceivers {
		if transceiver.Receiver() != nil {
			result = append(result, transceiver.Receiver())
		}
	}
	return result
}

// GetTransceivers returns the RTCRtpTransceiver that are currently attached to this RTCPeerConnection
func (pc *PeerConnection) GetTransceivers() []*RTPTransceiver {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	return pc.rtpTransceivers
}

// AddTrack adds a Track to the PeerConnection
/*
TODO:
	将track添加到PeerConnection的RTPTransceiver的RTPSender中
	首先查看是否存在可以被复用RTPTransceiver
	1、如果存在可用的RTPTransceiver，即其媒体类型与track相同，且RTPSender没有被使用，则该RTPTransceiver可以被复用
	2、否则新建可以新的RTPTransceiver
*/
func (pc *PeerConnection) AddTrack(track *Track) (*RTPSender, error) {
	if pc.isClosed.get() {
		return nil, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	var transceiver *RTPTransceiver
	//找到可用的Transceiver：类型一致且Sender没有被使用
	for _, t := range pc.GetTransceivers() {
		if !t.stopped && t.kind == track.Kind() && t.Sender() == nil {
			transceiver = t
			break
		}
	}
	//如果已经存在transceiver，直接复用
	if transceiver != nil {
		//新建一个RTPSender
		sender, err := pc.api.NewRTPSender(track, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}
		//设置该Transceiver的RTPSender
		transceiver.setSender(sender)
		// we still need to call setSendingTrack to ensure direction has changed
		//设置RTPSender对应的direction属性，如果是增加track，增加send属性；如果是移除track，移除send属性
		if err := transceiver.setSendingTrack(track); err != nil {
			return nil, err
		}
		return sender, nil
	}
	//如果不存在transceiver，新加
	transceiver, err := pc.AddTransceiverFromTrack(track)
	if err != nil {
		return nil, err
	}

	return transceiver.Sender(), nil
}

// AddTransceiver Create a new RTCRtpTransceiver and add it to the set of transceivers.
// Deprecated: Use AddTrack, AddTransceiverFromKind or AddTransceiverFromTrack
func (pc *PeerConnection) AddTransceiver(trackOrKind RTPCodecType, init ...RtpTransceiverInit) (*RTPTransceiver, error) {
	return pc.AddTransceiverFromKind(trackOrKind, init...)
}

// RemoveTrack removes a Track from the PeerConnection
func (pc *PeerConnection) RemoveTrack(sender *RTPSender) error {
	if pc.isClosed.get() {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	var transceiver *RTPTransceiver
	for _, t := range pc.GetTransceivers() {
		if t.Sender() == sender {
			transceiver = t
			break
		}
	}

	if transceiver == nil {
		return &rtcerr.InvalidAccessError{Err: ErrSenderNotCreatedByConnection}
	} else if err := sender.Stop(); err != nil {
		return err
	}

	return transceiver.setSendingTrack(nil)
}

// AddTransceiverFromKind Create a new RTCRtpTransceiver(SendRecv or RecvOnly) and add it to the set of transceivers.
func (pc *PeerConnection) AddTransceiverFromKind(kind RTPCodecType, init ...RtpTransceiverInit) (*RTPTransceiver, error) {
	if pc.isClosed.get() {
		return nil, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	direction := RTPTransceiverDirectionSendrecv
	if len(init) > 1 {
		return nil, fmt.Errorf("AddTransceiverFromKind only accepts one RtpTransceiverInit")
	} else if len(init) == 1 {
		direction = init[0].Direction
	}

	switch direction {
	case RTPTransceiverDirectionSendrecv:
		codecs := pc.api.mediaEngine.GetCodecsByKind(kind)
		if len(codecs) == 0 {
			return nil, fmt.Errorf("no %s codecs found", kind.String())
		}
		//指定track的ssrc、trackId以及trackLabel信息
		//如果是Sendrecv，需要初始化一个对应的track，并增加一个Transceiver
		track, err := pc.NewTrack(codecs[0].PayloadType, mathRand.Uint32(), util.RandSeq(trackDefaultIDLength), util.RandSeq(trackDefaultLabelLength))
		if err != nil {
			return nil, err
		}
		//添加一个Transceiver
		return pc.AddTransceiverFromTrack(track, init...)

	case RTPTransceiverDirectionRecvonly:
		//如果只是Recvonly，只需要初始化Receiver即可，无需track
		receiver, err := pc.api.NewRTPReceiver(kind, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}
		//添加一个Transceiver
		return pc.newRTPTransceiver(
			receiver,
			nil,
			RTPTransceiverDirectionRecvonly,
			kind,
		), nil
	default:
		return nil, fmt.Errorf("AddTransceiverFromKind currently only supports recvonly and sendrecv")
	}
}

// AddTransceiverFromTrack Creates a new send only transceiver and add it to the set of
func (pc *PeerConnection) AddTransceiverFromTrack(track *Track, init ...RtpTransceiverInit) (*RTPTransceiver, error) {
	if pc.isClosed.get() {
		return nil, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	direction := RTPTransceiverDirectionSendrecv
	if len(init) > 1 {
		return nil, fmt.Errorf("AddTransceiverFromTrack only accepts one RtpTransceiverInit")
	} else if len(init) == 1 {
		direction = init[0].Direction
	}

	switch direction {
	case RTPTransceiverDirectionSendrecv:
		receiver, err := pc.api.NewRTPReceiver(track.Kind(), pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		sender, err := pc.api.NewRTPSender(track, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		return pc.newRTPTransceiver(
			receiver,
			sender,
			RTPTransceiverDirectionSendrecv,
			track.Kind(),
		), nil

	case RTPTransceiverDirectionSendonly:
		sender, err := pc.api.NewRTPSender(track, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		return pc.newRTPTransceiver(
			nil,
			sender,
			RTPTransceiverDirectionSendonly,
			track.Kind(),
		), nil
	default:
		return nil, fmt.Errorf("AddTransceiverFromTrack currently only supports sendonly and sendrecv")
	}
}

// CreateDataChannel creates a new DataChannel object with the given label
// and optional DataChannelInit used to configure properties of the
// underlying channel such as data reliability.
func (pc *PeerConnection) CreateDataChannel(label string, options *DataChannelInit) (*DataChannel, error) {
	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #2)
	if pc.isClosed.get() {
		return nil, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	params := &DataChannelParameters{
		Label:   label,
		Ordered: true,
	}

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #19)
	if options != nil {
		params.ID = options.ID
	}

	if options != nil {
		// Ordered indicates if data is allowed to be delivered out of order. The
		// default value of true, guarantees that data will be delivered in order.
		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #9)
		if options.Ordered != nil {
			params.Ordered = *options.Ordered
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #7)
		if options.MaxPacketLifeTime != nil {
			params.MaxPacketLifeTime = options.MaxPacketLifeTime
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #8)
		if options.MaxRetransmits != nil {
			params.MaxRetransmits = options.MaxRetransmits
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #10)
		if options.Protocol != nil {
			params.Protocol = *options.Protocol
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #11)
		if len(params.Protocol) > 65535 {
			return nil, &rtcerr.TypeError{Err: ErrProtocolTooLarge}
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #12)
		if options.Negotiated != nil {
			params.Negotiated = *options.Negotiated
		}
	}

	d, err := pc.api.newDataChannel(params, pc.log)
	if err != nil {
		return nil, err
	}

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #16)
	if d.maxPacketLifeTime != nil && d.maxRetransmits != nil {
		return nil, &rtcerr.TypeError{Err: ErrRetransmitsOrPacketLifeTime}
	}

	pc.sctpTransport.lock.Lock()
	pc.sctpTransport.dataChannels = append(pc.sctpTransport.dataChannels, d)
	pc.sctpTransport.dataChannelsRequested++
	pc.sctpTransport.lock.Unlock()

	// If SCTP already connected open all the channels
	if pc.sctpTransport.State() == SCTPTransportStateConnected {
		if err = d.open(pc.sctpTransport); err != nil {
			return nil, err
		}
	}

	return d, nil
}

// SetIdentityProvider is used to configure an identity provider to generate identity assertions
func (pc *PeerConnection) SetIdentityProvider(provider string) error {
	return fmt.Errorf("TODO SetIdentityProvider")
}

// WriteRTCP sends a user provided RTCP packet to the connected peer
// If no peer is connected the packet is discarded
func (pc *PeerConnection) WriteRTCP(pkts []rtcp.Packet) error {
	raw, err := rtcp.Marshal(pkts)
	if err != nil {
		return err
	}

	srtcpSession, err := pc.dtlsTransport.getSRTCPSession()
	if err != nil {
		return nil
	}

	writeStream, err := srtcpSession.OpenWriteStream()
	if err != nil {
		return fmt.Errorf("WriteRTCP failed to open WriteStream: %v", err)
	}

	if _, err := writeStream.Write(raw); err != nil {
		return err
	}
	return nil
}

// Close ends the PeerConnection
func (pc *PeerConnection) Close() error {
	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #2)
	if pc.isClosed.get() {
		return nil
	}

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #3)
	pc.isClosed.set(true)

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #4)
	pc.signalingState = SignalingStateClosed

	// Try closing everything and collect the errors
	// Shutdown strategy:
	// 1. All Conn close by closing their underlying Conn.
	// 2. A Mux stops this chain. It won't close the underlying
	//    Conn if one of the endpoints is closed down. To
	//    continue the chain the Mux has to be closed.
	closeErrs := make([]error, 4)

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #5)
	for _, t := range pc.rtpTransceivers {
		closeErrs = append(closeErrs, t.Stop())
	}

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #6)
	if pc.sctpTransport != nil {
		pc.sctpTransport.lock.Lock()
		for _, d := range pc.sctpTransport.dataChannels {
			d.setReadyState(DataChannelStateClosed)
		}
		pc.sctpTransport.lock.Unlock()
	}

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #7)
	if pc.sctpTransport != nil {
		closeErrs = append(closeErrs, pc.sctpTransport.Stop())
	}

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #8)
	closeErrs = append(closeErrs, pc.dtlsTransport.Stop())

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #9,#10,#11)
	if pc.iceTransport != nil {
		closeErrs = append(closeErrs, pc.iceTransport.Stop())
	}

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #12)
	pc.updateConnectionState(pc.ICEConnectionState(), pc.dtlsTransport.State())

	return util.FlattenErrs(closeErrs)
}

// NewTrack Creates a new Track
func (pc *PeerConnection) NewTrack(payloadType uint8, ssrc uint32, id, label string) (*Track, error) {
	//这里只能使用mediaEngine中已经注册的codec，所以在注册mediaEngine前，应该需要清楚自己需要的媒体能力
	codec, err := pc.api.mediaEngine.getCodec(payloadType)
	if err != nil {
		return nil, err
	} else if codec.Payloader == nil {
		return nil, fmt.Errorf("codec payloader not set")
	}

	return NewTrack(payloadType, ssrc, id, label, codec)
}

func (pc *PeerConnection) newRTPTransceiver(
	receiver *RTPReceiver,
	sender *RTPSender,
	direction RTPTransceiverDirection,
	kind RTPCodecType,
) *RTPTransceiver {
	t := &RTPTransceiver{kind: kind}
	t.setReceiver(receiver)
	t.setSender(sender)
	t.setDirection(direction)

	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.rtpTransceivers = append(pc.rtpTransceivers, t)
	return t
}

// CurrentLocalDescription represents the local description that was
// successfully negotiated the last time the PeerConnection transitioned
// into the stable state plus any local candidates that have been generated
// by the ICEAgent since the offer or answer was created.
func (pc *PeerConnection) CurrentLocalDescription() *SessionDescription {
	return populateLocalCandidates(pc.currentLocalDescription, pc.iceGatherer, pc.ICEGatheringState())
}

// PendingLocalDescription represents a local description that is in the
// process of being negotiated plus any local candidates that have been
// generated by the ICEAgent since the offer or answer was created. If the
// PeerConnection is in the stable state, the value is null.
func (pc *PeerConnection) PendingLocalDescription() *SessionDescription {
	return populateLocalCandidates(pc.pendingLocalDescription, pc.iceGatherer, pc.ICEGatheringState())
}

// CurrentRemoteDescription represents the last remote description that was
// successfully negotiated the last time the PeerConnection transitioned
// into the stable state plus any remote candidates that have been supplied
// via AddICECandidate() since the offer or answer was created.
func (pc *PeerConnection) CurrentRemoteDescription() *SessionDescription {
	return pc.currentRemoteDescription
}

// PendingRemoteDescription represents a remote description that is in the
// process of being negotiated, complete with any remote candidates that
// have been supplied via AddICECandidate() since the offer or answer was
// created. If the PeerConnection is in the stable state, the value is
// null.
func (pc *PeerConnection) PendingRemoteDescription() *SessionDescription {
	return pc.pendingRemoteDescription
}

// SignalingState attribute returns the signaling state of the
// PeerConnection instance.
func (pc *PeerConnection) SignalingState() SignalingState {
	return pc.signalingState
}

// ICEGatheringState attribute returns the ICE gathering state of the
// PeerConnection instance.
func (pc *PeerConnection) ICEGatheringState() ICEGatheringState {
	if pc.iceGatherer == nil {
		return ICEGatheringStateNew
	}

	switch pc.iceGatherer.State() {
	case ICEGathererStateNew:
		return ICEGatheringStateNew
	case ICEGathererStateGathering:
		return ICEGatheringStateGathering
	default:
		return ICEGatheringStateComplete
	}
}

// ConnectionState attribute returns the connection state of the
// PeerConnection instance.
func (pc *PeerConnection) ConnectionState() PeerConnectionState {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	return pc.connectionState
}

// GetStats return data providing statistics about the overall connection
func (pc *PeerConnection) GetStats() StatsReport {
	var (
		dataChannelsAccepted  uint32
		dataChannelsClosed    uint32
		dataChannelsOpened    uint32
		dataChannelsRequested uint32
	)
	statsCollector := newStatsReportCollector()
	statsCollector.Collecting()

	pc.mu.Lock()
	if pc.iceGatherer != nil {
		pc.iceGatherer.collectStats(statsCollector)
	}
	if pc.iceTransport != nil {
		pc.iceTransport.collectStats(statsCollector)
	}

	if pc.sctpTransport != nil {
		pc.sctpTransport.lock.Lock()
		dataChannels := append([]*DataChannel{}, pc.sctpTransport.dataChannels...)
		dataChannelsAccepted = pc.sctpTransport.dataChannelsAccepted
		dataChannelsOpened = pc.sctpTransport.dataChannelsOpened
		dataChannelsRequested = pc.sctpTransport.dataChannelsRequested
		pc.sctpTransport.lock.Unlock()

		for _, d := range dataChannels {
			state := d.ReadyState()
			if state != DataChannelStateConnecting && state != DataChannelStateOpen {
				dataChannelsClosed++
			}

			d.collectStats(statsCollector)
		}
		pc.sctpTransport.collectStats(statsCollector)
	}

	stats := PeerConnectionStats{
		Timestamp:             statsTimestampNow(),
		Type:                  StatsTypePeerConnection,
		ID:                    pc.statsID,
		DataChannelsAccepted:  dataChannelsAccepted,
		DataChannelsClosed:    dataChannelsClosed,
		DataChannelsOpened:    dataChannelsOpened,
		DataChannelsRequested: dataChannelsRequested,
	}
	pc.mu.Unlock()

	statsCollector.Collect(stats.ID, stats)
	return statsCollector.Ready()
}

// Start all transports. PeerConnection now has enough state
func (pc *PeerConnection) startTransports(iceRole ICERole, dtlsRole DTLSRole, remoteUfrag, remotePwd, fingerprint, fingerprintHash string) {
	// Start the ice transport
	//开始ICE建连,主要逻辑在agent中
	pc.log.Debugf("startTransports: pc.iceTransport.Start\n")
	err := pc.iceTransport.Start(
		pc.iceGatherer,
		ICEParameters{
			UsernameFragment: remoteUfrag,
			Password:         remotePwd,
			ICELite:          false,
		},
		&iceRole,
	)
	if err != nil {
		pc.log.Warnf("Failed to start manager: %s", err)
		return
	}
	// Start the dtls transport
	//开始dtls交互:获取用于rtp/rtcp加密的key
	//并获取rtp/rtcp/dtls EndPoint
	/*
		1、ICE建连成功: 获取可用的数据传输通道
		2、DTLS建连成功: 客户端获取服务器的证书公钥，并使用公钥加密对称密钥发送给对端，于是双方获取对方的对称加密密钥
		3、RTP数据包使用对称加密密钥加密成SRTP后，通过ICE发送给对端，对端解密为RTP
		4、获取对端的SRTP，使用对称加密密钥解密为RTP数据包
	*/
	pc.log.Debugf("startTransports: pc.dtlsTransport.Start\n")
	err = pc.dtlsTransport.Start(DTLSParameters{
		DisableEncrypt: pc.api.settingEngine.disableEncrypt,
		Role:           dtlsRole,
		Fingerprints:   []DTLSFingerprint{{Algorithm: fingerprintHash, Value: fingerprint}},
	})
	pc.updateConnectionState(pc.ICEConnectionState(), pc.dtlsTransport.State())
	if err != nil {
		pc.log.Warnf("Failed to start manager: %s", err)
		return
	}
}

func (pc *PeerConnection) startRTP(isRenegotiation bool, remoteDesc *SessionDescription) {
	currentTransceivers := append([]*RTPTransceiver{}, pc.GetTransceivers()...)
	/*
		a=group:BUNDLE audio video data
		a=msid-semantic: WMS h1aZ20mbQB0GSsq0YxLfJmiYWE9CBfGch97C
		m=audio 9 UDP/TLS/RTP/SAVPF 111 103 104 9 0 8 106 105 13 126
		a=mid:audio
		a=ssrc:18509423 msid:h1aZ20mbQB0GSsq0YxLfJmiYWE9CBfGch97C 15598a91-caf9-4fff-a28f-3082310b2b7a
		m=video 9 UDP/TLS/RTP/SAVPF 100 101 107 116 117 96 97 99 98
		a=mid:video
		a=ssrc:3463951252 msid:h1aZ20mbQB0GSsq0YxLfJmiYWE9CBfGch97C ead4b4e9-b650-4ed5-86f8-6f5f5806346d
		m=application 9 DTLS/SCTP 5000
		a=mid:data
		TODO: 解析track
			对于Plan B模式，一个mLine对应若干个类型相同的track，属于一对多
			对于Unified Plan模式，一个mLine对应一个track，属于一对一
			每个<a=ssrc:ssrc_id msid:mediaStreamId trackId>中，指定了每个track的媒体源、所属mediaStream以及其trackId
	*/
	trackDetails := trackDetailsFromSDP(pc.log, remoteDesc.parsed)
	if isRenegotiation {
		/*
			TODO:重协商做了啥
				重协商主要针对remote track做了调整，remote track信息保存在RTPTransceiver的RTPReceiver中
				1、如果当前RTPTransceiver的RTPReceiver绑定了track，则查看该track是否仍然存在(ssrc为唯一id)
					1.1、track仍然使用中，更新该track信息，保留使用
					1.2、track已经不再使用，停止并新建一个替换之
				总之就是删除不再使用的RTPReceiver，保留并更新正在使用的RTPReceiver！！！
		*/
		//检查RTPTransceiver的RTPReceiver，确认是否仍在使用中，删除不再使用的
		for _, t := range currentTransceivers {
			//空的Receiver
			if t.Receiver() == nil || t.Receiver().Track() == nil {
				continue
			}

			t.Receiver().Track().mu.Lock()
			ssrc := t.Receiver().Track().ssrc
			//该Receiver仍有对应的track存在，更新属性即可
			if _, ok := trackDetails[ssrc]; ok {
				incoming := trackDetails[ssrc]
				t.Receiver().Track().id = incoming.id       //track_id
				t.Receiver().Track().label = incoming.label //stream_id
				t.Receiver().Track().mu.Unlock()
				continue
			}
			t.Receiver().Track().mu.Unlock()
			//如果Receiver已经不再使用，删除并重建空的
			if err := t.Receiver().Stop(); err != nil {
				pc.log.Warnf("Failed to stop RtpReceiver: %s", err)
				continue
			}
			//重建Receiver，此时Receiver的track为空，即不与任何track绑定
			receiver, err := pc.api.NewRTPReceiver(t.Receiver().kind, pc.dtlsTransport)
			if err != nil {
				pc.log.Warnf("Failed to create new RtpReceiver: %s", err)
				continue
			}
			t.setReceiver(receiver)
		}
	}
	//初始化Receivers并接收数据，其中会初始化RtpSession/RtcpSession，并获取对应的Stream对应
	//session从conn对象中循环获取裸数据，经过解密后Unmarshal为rtp/rtcp packet，写入到对应的Stream对象的buffer中
	//应用层读取数据操作其实是从buffer中获取的
	pc.startRTPReceivers(trackDetails, currentTransceivers)
	pc.startRTPSenders(currentTransceivers)
	//datachannel在第一次startRTP后就建立完毕(isRenegotiation=false作为第一次调用startRTP的标志)
	//之后的调用主要是重协商操作，所以之后的isRenegotiation=true
	if !isRenegotiation {
		//todo: 同时支持rtp/rtcp加密与不加密
		//drainRTP/drainSRTP专门用于处理接收sdp中undeclared ssrc对应的rtp/rtcp包数据
		if pc.api.settingEngine.disableEncrypt {
			pc.drainRTP()
		} else {
			pc.drainSRTP()
		}
		//m=application 使用datachannel传输
		if haveApplicationMediaSection(remoteDesc.parsed) {
			pc.startSCTP()
		}
	}
}

// GetRegisteredRTPCodecs gets a list of registered RTPCodec from the underlying constructed MediaEngine
func (pc *PeerConnection) GetRegisteredRTPCodecs(kind RTPCodecType) []*RTPCodec {
	return pc.api.mediaEngine.GetCodecsByKind(kind)
}

// generateUnmatchedSDP generates an SDP that doesn't take remote state into account
// This is used for the initial call for CreateOffer
func (pc *PeerConnection) generateUnmatchedSDP(useIdentity bool) (*sdp.SessionDescription, error) {
	d := sdp.NewJSEPSessionDescription(useIdentity)
	if err := addFingerprints(d, pc.configuration.Certificates[0]); err != nil {
		return nil, err
	}

	iceParams, err := pc.iceGatherer.GetLocalParameters()
	if err != nil {
		return nil, err
	}

	candidates, err := pc.iceGatherer.GetLocalCandidates()
	if err != nil {
		return nil, err
	}

	isPlanB := pc.configuration.SDPSemantics == SDPSemanticsPlanB
	mediaSections := []mediaSection{}

	if isPlanB {
		video := make([]*RTPTransceiver, 0)
		audio := make([]*RTPTransceiver, 0)

		for _, t := range pc.GetTransceivers() {
			if t.kind == RTPCodecTypeVideo {
				video = append(video, t)
			} else if t.kind == RTPCodecTypeAudio {
				audio = append(audio, t)
			}
			if t.Sender() != nil {
				t.Sender().setNegotiated()
			}
		}

		if len(video) > 0 {
			mediaSections = append(mediaSections, mediaSection{id: "video", transceivers: video})
		}
		if len(audio) > 0 {
			mediaSections = append(mediaSections, mediaSection{id: "audio", transceivers: audio})
		}
		mediaSections = append(mediaSections, mediaSection{id: "data", data: true})
	} else {
		for _, t := range pc.GetTransceivers() {
			if t.Sender() != nil {
				t.Sender().setNegotiated()
			}
			mediaSections = append(mediaSections, mediaSection{id: t.Mid(), transceivers: []*RTPTransceiver{t}})
		}

		mediaSections = append(mediaSections, mediaSection{id: strconv.Itoa(len(mediaSections)), data: true})
	}

	return populateSDP(d, isPlanB, pc.api.settingEngine.candidates.ICELite, pc.api.mediaEngine, connectionRoleFromDtlsRole(defaultDtlsRoleOffer), candidates, iceParams, mediaSections, pc.ICEGatheringState())
}

// generateMatchedSDP generates a SDP and takes the remote state into account
// this is used everytime we have a RemoteDescription
func (pc *PeerConnection) generateMatchedSDP(useIdentity bool, includeUnmatched bool, connectionRole sdp.ConnectionRole) (*sdp.SessionDescription, error) {
	//构造基本SDP框架
	d := sdp.NewJSEPSessionDescription(useIdentity)
	if err := addFingerprints(d, pc.configuration.Certificates[0]); err != nil {
		return nil, err
	}

	iceParams, err := pc.iceGatherer.GetLocalParameters()
	if err != nil {
		return nil, err
	}

	candidates, err := pc.iceGatherer.GetLocalCandidates()
	if err != nil {
		return nil, err
	}

	var t *RTPTransceiver
	//获取localTransceivers，可以根据Transceivers获取track信息
	localTransceivers := append([]*RTPTransceiver{}, pc.GetTransceivers()...)
	detectedPlanB := descriptionIsPlanB(pc.RemoteDescription())
	mediaSections := []mediaSection{}

	for _, media := range pc.RemoteDescription().parsed.MediaDescriptions {
		midValue := getMidValue(media)
		if midValue == "" {
			return nil, fmt.Errorf("RemoteDescription contained media section without mid value")
		}
		//datachannel单独添加，因为一个回话最多只有一个datachannel
		if media.MediaName.Media == mediaSectionApplication {
			mediaSections = append(mediaSections, mediaSection{id: midValue, data: true})
			continue
		}
		//根据remote媒体type和direction，获取local媒体type(audio/video)和direction
		//local媒体类型与remote媒体类型一致，但是direction一般为相反(单向时，SendOnly或RecvOnly)或者相同(SendRecv)
		kind := NewRTPCodecType(media.MediaName.Media)
		direction := getPeerDirection(media)
		if kind == 0 || direction == RTPTransceiverDirection(Unknown) {
			continue
		}
		//定义的SDP type,而detectedPlanB是根据实际sdp检测出来的，下面会将两者进行对比，如果不相符，则返回错误
		sdpSemantics := pc.configuration.SDPSemantics

		switch {
		//PlanB Offer
		case sdpSemantics == SDPSemanticsPlanB || sdpSemantics == SDPSemanticsUnifiedPlanWithFallback && detectedPlanB:
			if !detectedPlanB {
				return nil, &rtcerr.TypeError{Err: ErrIncorrectSDPSemantics}
			}
			// If we're responding to a plan-b offer, then we should try to fill up this
			// media entry with all matching local transceivers
			mediaTransceivers := []*RTPTransceiver{}
			//如果是PlanB模式，每次遍历出一组audio或者video，需要将其全部解析完毕
			for {
				// keep going until we can't get any more
				//根据remote type和direction，找出符合条件的local transceiver，每次只返回一个transceiver，直到返回nil
				t, localTransceivers = satisfyTypeAndDirection(kind, direction, localTransceivers)
				if t == nil {
					//如果没找到任何合适的，则置direction为Inactive
					if len(mediaTransceivers) == 0 {
						t = &RTPTransceiver{kind: kind}
						t.setDirection(RTPTransceiverDirectionInactive)
						mediaTransceivers = append(mediaTransceivers, t)
					}
					break
				}
				//如果Sender不为空，则设置Negotiated=true，即已协商
				if t.Sender() != nil {
					t.Sender().setNegotiated()
				}
				mediaTransceivers = append(mediaTransceivers, t)
			}
			mediaSections = append(mediaSections, mediaSection{id: midValue, transceivers: mediaTransceivers})
		case sdpSemantics == SDPSemanticsUnifiedPlan || sdpSemantics == SDPSemanticsUnifiedPlanWithFallback:
			if detectedPlanB {
				return nil, &rtcerr.TypeError{Err: ErrIncorrectSDPSemantics}
			}
			//如果是UnifiedPlan模式，每次遍历出单个audio或video，每次只解析一条
			t, localTransceivers = findByMid(midValue, localTransceivers)
			if t == nil {
				return nil, fmt.Errorf("cannot find transceiver with mid %q", midValue)
			}
			if t.Sender() != nil {
				t.Sender().setNegotiated()
			}
			mediaTransceivers := []*RTPTransceiver{t}
			mediaSections = append(mediaSections, mediaSection{id: midValue, transceivers: mediaTransceivers})
		}
	}

	// If we are offering also include unmatched local transceivers
	if !detectedPlanB && includeUnmatched {
		for _, t := range localTransceivers {
			if t.Sender() != nil {
				t.Sender().setNegotiated()
			}
			mediaSections = append(mediaSections, mediaSection{id: t.Mid(), transceivers: []*RTPTransceiver{t}})
		}
	}

	if pc.configuration.SDPSemantics == SDPSemanticsUnifiedPlanWithFallback && detectedPlanB {
		pc.log.Info("Plan-B Offer detected; responding with Plan-B Answer")
	}

	return populateSDP(d, detectedPlanB, pc.api.settingEngine.candidates.ICELite, pc.api.mediaEngine, connectionRole, candidates, iceParams, mediaSections, pc.ICEGatheringState())
}
