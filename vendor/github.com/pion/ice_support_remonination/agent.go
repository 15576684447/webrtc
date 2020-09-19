// Package ice implements the Interactive Connectivity Establishment (ICE)
// protocol defined in rfc5245.
package ice

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/mdns"
	"github.com/pion/stun"
	"github.com/pion/transport/vnet"
	"golang.org/x/net/ipv4"
)

const (
	// taskLoopInterval is the interval at which the agent performs checks
	defaultTaskLoopInterval = 2 * time.Second

	// keepaliveInterval used to keep candidates alive
	defaultKeepaliveInterval = 10 * time.Second

	// weakKeepaliveInterval used to keep candidates alive
	defaultWeakKeepaliveInterval = 20 * time.Second

	// defaultConnectionTimeout used to declare a connection dead
	defaultConnectionTimeout = 30 * time.Second

	// timeout for candidate selection, after this time, the best candidate is used
	defaultCandidateSelectionTimeout = 10 * time.Second

	// wait time before nominating a host candidate
	defaultHostAcceptanceMinWait = 0

	// wait time before nominating a srflx candidate
	defaultSrflxAcceptanceMinWait = 500 * time.Millisecond

	// wait time before nominating a prflx candidate
	defaultPrflxAcceptanceMinWait = 1000 * time.Millisecond

	// wait time before nominating a relay candidate
	defaultRelayAcceptanceMinWait = 2000 * time.Millisecond

	// max binding request before considering a pair failed
	defaultMaxBindingRequests = 7

	// the number of bytes that can be buffered before we start to error
	maxBufferSize = 1000 * 1000 // 1MB

	// wait time before binding requests can be deleted
	maxBindingRequestTimeout = 8000 * time.Millisecond

	// renomination interval
	defaultRenominationMinInterval = 5 * time.Second

	// start check pace interval
	startConnectivityCheckPaceTime = 20

	// A is better
	aIsBetter = 1
	// B is better
	bIsBetter = -1
	// A and B is equal
	aAndBEqual = 0
)

var (
	defaultCandidateTypes = []CandidateType{CandidateTypeHost, CandidateTypeServerReflexive, CandidateTypeRelay}
)

type bindingRequest struct {
	timestamp      time.Time
	transactionID  [stun.TransactionIDSize]byte
	destination    net.Addr
	isUseCandidate bool
}

type DataHandler interface {
	Process([]byte) (int, error)
}

// Agent represents the ICE agent
type Agent struct {
	onConnectionStateChangeHdlr       func(ConnectionState)
	onSelectedCandidatePairChangeHdlr func(Candidate, Candidate, SwitchReason)
	onCandidateHdlr                   func(Candidate)

	// Used to block double Dial/Accept
	opened bool

	// State owned by the taskLoop
	taskChan        chan task
	onConnected     chan struct{}
	onConnectedOnce sync.Once

	connectivityTicker *time.Ticker
	// force candidate to be contacted immediately (instead of waiting for connectivityTicker)
	forceCandidateContact chan bool

	trickle    bool
	tieBreaker uint64
	lite       bool

	connectionState ConnectionState
	gatheringState  GatheringState

	mDNSMode MulticastDNSMode
	mDNSName string
	mDNSConn *mdns.Conn

	haveStarted   atomic.Value
	isControlling bool

	maxBindingRequests uint16

	candidateSelectionTimeout time.Duration
	hostAcceptanceMinWait     time.Duration
	srflxAcceptanceMinWait    time.Duration
	prflxAcceptanceMinWait    time.Duration
	relayAcceptanceMinWait    time.Duration
	renominationMinInterval   time.Duration

	portmin uint16
	portmax uint16

	candidateTypes []CandidateType

	// How long should a pair stay quiet before we declare it dead?
	// 0 means never timeout
	connectionTimeout time.Duration

	// How often should we send keepalive packets?
	// 0 means never
	keepaliveInterval     time.Duration
	weakKeepaliveInterval time.Duration

	// How after should we run our internal taskLoop
	taskLoopInterval time.Duration

	localUfrag      string
	localPwd        string
	localCandidates map[NetworkType][]Candidate

	remoteUfrag      string
	remotePwd        string
	remoteCandidates map[NetworkType][]Candidate

	udpGodPorts         []uint16
	supportRenomination bool
	maxGeneration       int
	frequentlyCheck     bool
	frequentlyTimer     time.Duration
	timeBeforeNow       TimeFromNow

	checklist []*candidatePair
	selector  pairCandidateSelector

	selectedPairMutex sync.RWMutex
	selectedPair      *candidatePair
	lastSelectedPair  time.Time

	urls         []*URL
	networkTypes []NetworkType
	ips          map[string]int

	buffer      *Buffer
	dataHandler DataHandler

	// LRU of outbound Binding request Transaction IDs
	pendingBindingRequests []bindingRequest

	// 1:1 D-NAT IP address mapping
	extIPMapper *externalIPMapper

	// State for closing
	done chan struct{}
	err  atomicError

	loggerFactory logging.LoggerFactory
	log           logging.LeveledLogger

	net *vnet.Net

	timerWorker TimerInterface

	interfaceFilter func(string) bool
	onEventHdlr     func(event Event, local string, remote string)
}

func (a *Agent) ok() error {
	select {
	case <-a.done:
		return a.getErr()
	default:
	}
	return nil
}

func (a *Agent) getErr() error {
	err := a.err.Load()
	if err != nil {
		return err
	}
	return ErrClosed
}

// AgentConfig collects the arguments to ice.Agent construction into
// a single structure, for future-proofness of the interface
type AgentConfig struct {
	Urls []*URL

	// PortMin and PortMax are optional. Leave them 0 for the default UDP port allocation strategy.
	PortMin uint16
	PortMax uint16

	// Trickle specifies whether or not ice agent should trickle candidates or
	// work perform synchronous gathering.
	Trickle bool

	// MulticastDNSMode controls mDNS behavior for the ICE agent
	MulticastDNSMode MulticastDNSMode

	// ConnectionTimeout defaults to 30 seconds when this property is nil.
	// If the duration is 0, we will never timeout this connection.
	ConnectionTimeout *time.Duration
	// KeepaliveInterval determines how often should we send ICE
	// keepalives (should be less then connectiontimeout above)
	// when this is nil, it defaults to 10 seconds.
	// A keepalive interval of 0 means we never send keepalive packets
	KeepaliveInterval     *time.Duration
	WeakKeepaliveInterval *time.Duration

	// NetworkTypes is an optional configuration for disabling or enabling
	// support for specific network types.
	NetworkTypes []NetworkType

	// CandidateTypes is an optional configuration for disabling or enabling
	// support for specific candidate types.
	CandidateTypes []CandidateType

	LoggerFactory logging.LoggerFactory

	// taskLoopInterval controls how often our internal task loop runs, this
	// task loop handles things like sending keepAlives. This is only value for testing
	// keepAlive behavior should be modified with KeepaliveInterval and ConnectionTimeout
	taskLoopInterval time.Duration

	// MaxBindingRequests is the max amount of binding requests the agent will send
	// over a candidate pair for validation or nomination, if after MaxBindingRequests
	// the candidate is yet to answer a binding request or a nomination we set the pair as failed
	MaxBindingRequests *uint16

	// CandidatesSelectionTimeout specify a timeout for selecting candidates, if no nomination has happen
	// before this timeout, once hit we will nominate the best valid candidate available,
	// or mark the connection as failed if no valid candidate is available
	CandidateSelectionTimeout *time.Duration

	// Lite agents do not perform connectivity check and only provide host candidates.
	Lite bool

	// NAT1To1IPCandidateType is used along with NAT1To1IPs to specify which candidate type
	// the 1:1 NAT IP addresses should be mapped to.
	// If unspecified or CandidateTypeHost, NAT1To1IPs are used to replace host candidate IPs.
	// If CandidateTypeServerReflexive, it will insert a srflx candidate (as if it was dervied
	// from a STUN server) with its port number being the one for the actual host candidate.
	// Other values will result in an error.
	NAT1To1IPCandidateType CandidateType

	// NAT1To1IPs contains a list of public IP addresses that are to be used as a host
	// candidate or srflx candidate. This is used typically for servers that are behind
	// 1:1 D-NAT (e.g. AWS EC2 instances) and to eliminate the need of server reflexisive
	// candidate gathering.
	NAT1To1IPs []string

	// HostAcceptanceMinWait specify a minimum wait time before selecting host candidates
	HostAcceptanceMinWait *time.Duration
	// HostAcceptanceMinWait specify a minimum wait time before selecting srflx candidates
	SrflxAcceptanceMinWait *time.Duration
	// HostAcceptanceMinWait specify a minimum wait time before selecting prflx candidates
	PrflxAcceptanceMinWait *time.Duration
	// HostAcceptanceMinWait specify a minimum wait time before selecting relay candidates
	RelayAcceptanceMinWait *time.Duration

	// RenominationMinInterval specify minimum renomination interval
	RenominationMinInterval *time.Duration
	// Net is the our abstracted network interface for internal development purpose only
	// (see github.com/pion/transport/vnet)
	Net *vnet.Net
	Ips map[string]int

	UDPGodPorts         []uint16
	RemoteUfrag         string
	RemotePassword      string
	SupportRenomination bool
	// InterfaceFilter is a function that you can use in order to  whitelist or blacklist
	// the interfaces which are used to gather ICE candidates.
	InterfaceFilter func(string) bool

	LocalUsername string
	LocalPassword string
}

func containsCandidateType(candidateType CandidateType, candidateTypeList []CandidateType) bool {
	if candidateTypeList == nil {
		return false
	}
	for _, ct := range candidateTypeList {
		if ct == candidateType {
			return true
		}
	}
	return false
}

func createMulticastDNS(mDNSMode MulticastDNSMode, mDNSName string, log logging.LeveledLogger) (*mdns.Conn, MulticastDNSMode, error) {
	if mDNSMode == MulticastDNSModeDisabled {
		return nil, mDNSMode, nil
	}

	addr, mdnsErr := net.ResolveUDPAddr("udp4", mdns.DefaultAddress)
	if mdnsErr != nil {
		return nil, mDNSMode, mdnsErr
	}

	l, mdnsErr := net.ListenUDP("udp4", addr)
	if mdnsErr != nil {
		// If ICE fails to start MulticastDNS server just warn the user and continue
		log.Errorf("Failed to enable mDNS, continuing in mDNS disabled mode: (%s)", mdnsErr)
		return nil, MulticastDNSModeDisabled, nil
	}

	switch mDNSMode {
	case MulticastDNSModeQueryOnly:
		conn, err := mdns.Server(ipv4.NewPacketConn(l), &mdns.Config{})
		return conn, mDNSMode, err
	case MulticastDNSModeQueryAndGather:
		conn, err := mdns.Server(ipv4.NewPacketConn(l), &mdns.Config{
			LocalNames: []string{mDNSName},
		})
		return conn, mDNSMode, err
	default:
		return nil, mDNSMode, nil
	}
}

// NewAgent creates a new Agent
func NewAgent(config *AgentConfig) (*Agent, error) {
	if config.PortMax < config.PortMin {
		return nil, ErrPort
	}

	mDNSName, err := generateMulticastDNSName()
	if err != nil {
		return nil, err
	}

	mDNSMode := config.MulticastDNSMode
	if mDNSMode == 0 {
		mDNSMode = MulticastDNSModeQueryOnly
	}

	loggerFactory := config.LoggerFactory
	if loggerFactory == nil {
		loggerFactory = logging.NewDefaultLoggerFactory()
	}
	log := loggerFactory.NewLogger("ice")

	var mDNSConn *mdns.Conn
	mDNSConn, mDNSMode, err = createMulticastDNS(mDNSMode, mDNSName, log)
	if err != nil {
		return nil, err
	}
	closeMDNSConn := func() {
		if mDNSConn != nil {
			if mdnsCloseErr := mDNSConn.Close(); mdnsCloseErr != nil {
				log.Warnf("Failed to close mDNS: %v", mdnsCloseErr)
			}
		}
	}

	ufrag := config.LocalUsername
	pwd := config.LocalPassword
	if ufrag == "" {
		ufrag = randSeq(16)
	}
	if pwd == "" {
		pwd = randSeq(32)
	}

	a := &Agent{
		tieBreaker:             rand.New(rand.NewSource(time.Now().UnixNano())).Uint64(),
		lite:                   config.Lite,
		gatheringState:         GatheringStateNew,
		connectionState:        ConnectionStateNew,
		localCandidates:        make(map[NetworkType][]Candidate),
		remoteCandidates:       make(map[NetworkType][]Candidate),
		pendingBindingRequests: make([]bindingRequest, 0),
		checklist:              make([]*candidatePair, 0),
		urls:                   config.Urls,
		networkTypes:           config.NetworkTypes,

		localUfrag:    ufrag,
		localPwd:      pwd,
		taskChan:      make(chan task),
		onConnected:   make(chan struct{}),
		buffer:        NewBuffer(),
		dataHandler:   nil,
		done:          make(chan struct{}),
		portmin:       config.PortMin,
		portmax:       config.PortMax,
		trickle:       config.Trickle,
		loggerFactory: loggerFactory,
		log:           log,
		net:           config.Net,
		ips:           config.Ips,
		udpGodPorts:   config.UDPGodPorts,

		remoteUfrag: config.RemoteUfrag,
		remotePwd:   config.RemotePassword,

		mDNSMode: mDNSMode,
		mDNSName: mDNSName,
		mDNSConn: mDNSConn,

		forceCandidateContact: make(chan bool, 1),

		interfaceFilter:     config.InterfaceFilter,
		supportRenomination: config.SupportRenomination,
	}
	a.haveStarted.Store(false)

	if a.net == nil {
		a.net = vnet.NewNet(nil)
	} else if a.net.IsVirtual() {
		a.log.Warn("vnet is enabled")
		if a.mDNSMode != MulticastDNSModeDisabled {
			a.log.Warn("vnet does not support mDNS yet")
		}
	}

	a.initWithDefaults(config)

	// Make sure the buffer doesn't grow indefinitely.
	// NOTE: We actually won't get anywhere close to this limit.
	// SRTP will constantly read from the endpoint and drop packets if it's full.
	a.buffer.SetLimitSize(maxBufferSize)

	if a.lite && (len(a.candidateTypes) != 1 || a.candidateTypes[0] != CandidateTypeHost) {
		closeMDNSConn()
		return nil, ErrLiteUsingNonHostCandidates
	}

	if config.Urls != nil && len(config.Urls) > 0 && !containsCandidateType(CandidateTypeServerReflexive, a.candidateTypes) && !containsCandidateType(CandidateTypeRelay, a.candidateTypes) {
		closeMDNSConn()
		return nil, ErrUselessUrlsProvided
	}

	if err = a.initExtIPMapping(config); err != nil {
		closeMDNSConn()
		return nil, err
	}
	a.timerWorker = globalTimer.GetRandomWorker()
	a.timeBeforeNow = &timeBeforeNow{}

	// Initialize local candidates
	//TODO: 搜集本地candidate入口函数
	if !a.trickle {
		a.gatherCandidates()
	}
	return a, nil
}

// a sSeparate init routine called by NewAgent() to overcome gocyclo error with golangci-lint
func (a *Agent) initWithDefaults(config *AgentConfig) {
	if config.MaxBindingRequests == nil {
		a.maxBindingRequests = defaultMaxBindingRequests
	} else {
		a.maxBindingRequests = *config.MaxBindingRequests
	}

	if config.CandidateSelectionTimeout == nil {
		a.candidateSelectionTimeout = defaultCandidateSelectionTimeout
	} else {
		a.candidateSelectionTimeout = *config.CandidateSelectionTimeout
	}

	if config.HostAcceptanceMinWait == nil {
		a.hostAcceptanceMinWait = defaultHostAcceptanceMinWait
	} else {
		a.hostAcceptanceMinWait = *config.HostAcceptanceMinWait
	}

	if config.SrflxAcceptanceMinWait == nil {
		a.srflxAcceptanceMinWait = defaultSrflxAcceptanceMinWait
	} else {
		a.srflxAcceptanceMinWait = *config.SrflxAcceptanceMinWait
	}

	if config.PrflxAcceptanceMinWait == nil {
		a.prflxAcceptanceMinWait = defaultPrflxAcceptanceMinWait
	} else {
		a.prflxAcceptanceMinWait = *config.PrflxAcceptanceMinWait
	}

	if config.RelayAcceptanceMinWait == nil {
		a.relayAcceptanceMinWait = defaultRelayAcceptanceMinWait
	} else {
		a.relayAcceptanceMinWait = *config.RelayAcceptanceMinWait
	}

	// connectionTimeout used to declare a connection dead
	if config.ConnectionTimeout == nil {
		a.connectionTimeout = defaultConnectionTimeout
	} else {
		a.connectionTimeout = *config.ConnectionTimeout
	}

	if config.KeepaliveInterval == nil {
		a.keepaliveInterval = defaultKeepaliveInterval
	} else {
		a.keepaliveInterval = *config.KeepaliveInterval
	}

	if config.WeakKeepaliveInterval == nil {
		a.weakKeepaliveInterval = defaultWeakKeepaliveInterval
	} else {
		a.weakKeepaliveInterval = *config.WeakKeepaliveInterval
	}

	if config.RenominationMinInterval == nil {
		a.renominationMinInterval = defaultRenominationMinInterval
	} else {
		a.renominationMinInterval = *config.RenominationMinInterval
	}

	if config.taskLoopInterval == 0 {
		a.taskLoopInterval = defaultTaskLoopInterval
	} else {
		a.taskLoopInterval = config.taskLoopInterval
	}

	if config.CandidateTypes == nil || len(config.CandidateTypes) == 0 {
		a.candidateTypes = defaultCandidateTypes
	} else {
		a.candidateTypes = config.CandidateTypes
	}
	a.log.Debugf("SupportRenomination:%t", a.supportRenomination)
}

func (a *Agent) initExtIPMapping(config *AgentConfig) error {
	var err error
	a.extIPMapper, err = newExternalIPMapper(config.NAT1To1IPCandidateType, config.NAT1To1IPs)
	if err != nil {
		return err
	}
	if a.extIPMapper == nil {
		return nil // this may happen when config.NAT1To1IPs is an empty array
	}
	if a.extIPMapper.candidateType == CandidateTypeHost {
		if a.mDNSMode == MulticastDNSModeQueryAndGather {
			return ErrMulticastDNSWithNAT1To1IPMapping
		}
		candiHostEnabled := false
		for _, candiType := range a.candidateTypes {
			if candiType == CandidateTypeHost {
				candiHostEnabled = true
				break
			}
		}
		if !candiHostEnabled {
			return ErrIneffectiveNAT1To1IPMappingHost
		}

	} else if a.extIPMapper.candidateType == CandidateTypeServerReflexive {
		candiSrflxEnabled := false
		for _, candiType := range a.candidateTypes {
			if candiType == CandidateTypeServerReflexive {
				candiSrflxEnabled = true
				break
			}
		}
		if !candiSrflxEnabled {
			return ErrIneffectiveNAT1To1IPMappingSrflx
		}
	}
	return nil
}

// OnIceEvent sets a handler that is fired when the connection state changes
func (a *Agent) OnIceEvent(f func(event Event, local string, remote string)) error {
	return a.run(func(agent *Agent) {
		agent.onEventHdlr = f
	})
}

// OnConnectionStateChange sets a handler that is fired when the connection state changes
func (a *Agent) OnConnectionStateChange(f func(ConnectionState)) error {
	return a.run(func(agent *Agent) {
		agent.onConnectionStateChangeHdlr = f
	})
}

// OnSelectedCandidatePairChange sets a handler that is fired when the final candidate
// pair is selected
func (a *Agent) OnSelectedCandidatePairChange(f func(Candidate, Candidate, SwitchReason)) error {
	return a.run(func(agent *Agent) {
		agent.onSelectedCandidatePairChangeHdlr = f
	})
}

// OnCandidate sets a handler that is fired when new candidates gathered. When
// the gathering process complete the last candidate is nil.
func (a *Agent) OnCandidate(f func(Candidate)) error {
	return a.run(func(agent *Agent) {
		agent.onCandidateHdlr = f
	})
}

func (a *Agent) onSelectedCandidatePairChange(reason SwitchReason, p *candidatePair) {
	if p != nil {
		if a.onSelectedCandidatePairChangeHdlr != nil {
			a.onSelectedCandidatePairChangeHdlr(p.local, p.remote, reason)
		}
	}
}

func (a *Agent) startConnectivityChecks(isControlling bool, remoteUfrag, remotePwd string) error {
	switch {
	case a.haveStarted.Load():
		return ErrMultipleStart
	case remoteUfrag == "":
		if isControlling {
			return ErrRemoteUfragEmpty
		}
	case remotePwd == "":
		if isControlling {
			return ErrRemotePwdEmpty
		}
	}

	a.haveStarted.Store(true)
	a.log.Debugf("Started agent: isControlling? %t, remoteUfrag: %q, remotePwd: %q", isControlling, remoteUfrag, remotePwd)

	return a.run(func(agent *Agent) {
		agent.isControlling = isControlling
		agent.remoteUfrag = remoteUfrag
		agent.remotePwd = remotePwd

		if isControlling {
			a.selector = &controllingSelector{agent: a, log: a.log}
		} else {
			a.selector = &controlledSelector{agent: a, log: a.log}
		}

		if a.lite {
			a.selector = &liteSelector{pairCandidateSelector: a.selector}
		}

		a.selector.Start()
		a.tickLoop(time.Duration(0))
		agent.updateConnectionState(ConnectionStateChecking)

		// TODO this should be dynamic, and grow when the connection is stable
		a.requestConnectivityCheck()
		agent.connectivityTicker = time.NewTicker(a.taskLoopInterval)
		if agent.remoteUfrag != "" && agent.remotePwd != "" {
			a.pacedCheck(startConnectivityCheckPaceTime, 0)
		}
	})
}

func (a *Agent) pacedCheck(delta int, times int) {
	timeout := time.Duration(delta) * time.Millisecond
	a.timerWorker.DelayTimerTask(timeout, func() {

		select {
		case <-a.onConnected:
			return
		case <-a.done:
			return
		default:

		}

		if a.connectionState != ConnectionStateChecking {
			return
		}
		a.requestConnectivityCheck()
		times++
		if times == 6 {
			times = 0
			if 2*timeout > 2*time.Second {
				delta = 2000
				timeout = 2 * time.Second
			} else {
				delta = 2 * delta
				timeout = 2 * timeout
			}
		}

		a.pacedCheck(delta, times)

	})
}

func (a *Agent) checkConnectionState() bool {
	isInChecking := false
	for _, p := range a.checklist {
		if p.state != CandidatePairStateFailed && p.state != CandidatePairStateSucceeded {
			isInChecking = true
		}
	}
	if !isInChecking && a.connectionState == ConnectionStateConnected {
		a.updateConnectionState(ConnectionStateCompleted)
		return true
	}
	return false
}

func (a *Agent) updateConnectionState(newState ConnectionState) {
	if a.connectionState != newState {
		a.log.Infof("Setting new connection state: %s", newState)
		a.connectionState = newState
		hdlr := a.onConnectionStateChangeHdlr
		if hdlr != nil {
			// Call handler async since we may be holding the agent lock
			// and the handler may also require it
			go hdlr(newState)
		}
	}
}

func (a *Agent) checkAndSetSelectedPair(p *candidatePair) bool {
	a.selectedPairMutex.Lock()
	defer a.selectedPairMutex.Unlock()
	if a.selectedPair.Equal(p) {
		return false
	}
	//切换selectedPair时，先设置原先selectedPair的nominated=false，然后切换为新的pair，并设置nominated=true
	if a.selectedPair != nil {
		a.selectedPair.nominated = false
	}
	a.selectedPair = p
	//!!!注意，每次set新的selectedPair，都会设置nominated = true，即立即开始重提名
	a.selectedPair.nominated = true
	return true
}

func (a *Agent) setSelectedPair(reason SwitchReason, p *candidatePair) {
	if !a.checkAndSetSelectedPair(p) {
		return
	}
	a.log.Tracef("Set selected candidate pair: %s, reason:%d", p, reason)

	a.callback(SetSelectedPair, p.local.String(), p.remote.String())
	//设置本次setSelectedPair时刻
	a.lastSelectedPair = time.Now()
	// Notify when the selected pair changes
	//selectedPair改变后触发的回调
	a.onSelectedCandidatePairChange(reason, p)
	a.updateConnectionState(ConnectionStateConnected)

	// Close mDNS Conn. We don't need to do anymore querying
	// and no reason to respond to others traffic
	a.closeMulticastConn()

	// Signal connected
	a.onConnectedOnce.Do(func() { close(a.onConnected) })
}

func (a *Agent) pingAllCandidates() {
	for _, p := range a.checklist {

		if p.state == CandidatePairStateWaiting {
			p.state = CandidatePairStateInProgress
		} else if p.state != CandidatePairStateInProgress {
			continue
		}

		if p.bindingRequestCount > a.maxBindingRequests {
			a.log.Debugf("max requests reached for pair %s, marking it as failed\n", p)
			p.state = CandidatePairStateFailed
			p.pingTimes = 0
		} else if time.Since(p.lastPingTime) > (startConnectivityCheckPaceTime-5)*time.Millisecond {
			//the 15ms is less than the start frequently ping time 20ms
			a.selector.PingCandidate(p)
			p.bindingRequestCount++
		}
	}
}

func (a *Agent) getBestAvailableCandidatePair() *candidatePair {
	var best *candidatePair
	for _, p := range a.checklist {
		if p.state == CandidatePairStateFailed {
			continue
		}

		if best == nil {
			best = p
		} else if best.Priority() < p.Priority() {
			best = p
		}
	}
	return best
}

func (a *Agent) getBestValidCandidatePair() *candidatePair {
	var best *candidatePair
	for _, p := range a.checklist {
		if p.state != CandidatePairStateSucceeded {
			continue
		}

		if best == nil {
			best = p
		} else if a.CompareAB(best, p) == bIsBetter {
			best = p
		}
	}
	a.log.Debugf("Get best candidate pair %s in %s, maxGeneration:%d", best, a.checklist, a.maxGeneration)
	return best
}

func (a *Agent) CompareAB(A, B *candidatePair) int {
	aPriority := A.Priority()
	bPriority := B.Priority()
	if aPriority > bPriority {
		return aIsBetter
	} else if aPriority < bPriority {
		return bIsBetter
	}

	aGeneration := A.Generation(a.maxGeneration)
	bGeneration := B.Generation(a.maxGeneration)
	if aGeneration > bGeneration {
		return aIsBetter
	} else if aGeneration < bGeneration {
		return bIsBetter
	}
	if A.Attrs().LostRate < B.Attrs().LostRate {
		return aIsBetter
	} else if A.Attrs().LostRate > B.Attrs().LostRate {
		return bIsBetter
	}
	if A.Attrs().Rtt < B.Attrs().Rtt-10 {
		return aIsBetter
	} else if A.Attrs().Rtt > B.Attrs().Rtt+10 {
		return bIsBetter
	}
	if A.Attrs().NetJitter < B.Attrs().NetJitter-5 {
		return aIsBetter
	} else if A.Attrs().NetJitter > B.Attrs().NetJitter+5 {
		return bIsBetter
	}
	return aAndBEqual
}

func (a *Agent) addPair(local, remote Candidate) *candidatePair {
	p := newCandidatePair(local, remote, a.isControlling)
	have := false
	for _, pair := range a.checklist {
		if pair.Equal(p) {
			have = true
			pair.remote = remote
			pair.local = local
			continue
		}
	}
	if !have {
		a.checklist = append(a.checklist, p)
		a.log.Debugf("Add candidate pair:%s", p)
	}
	return p
}

func (a *Agent) findPair(local, remote Candidate) *candidatePair {
	for _, p := range a.checklist {
		if p.local.Equal(local) && p.remote.Equal(remote) {
			return p
		}
	}
	return nil
}

type task func(*Agent)

func (a *Agent) run(t task) error {
	err := a.ok()
	if err != nil {
		return err
	}

	select {
	case <-a.done:
		return a.getErr()
	default:
		a.timerWorker.RunInWorker(func() {
			t(a)
		})
	}
	return nil
}

func (a *Agent) tickLoop(delta time.Duration) {
	a.timerWorker.DelayTimerTask(delta, func() {
		select {
		case <-a.done:
			return
		default:
		}
		a.selector.ContactCandidates()
		a.tickLoop(2 * time.Second)

	})
}

func (a *Agent) frequentlyCheckKeepalive() {
	if a.frequentlyCheck {
		a.log.Debugf("Renomination-frequently check other pair")
		hivePair := false
		for _, p := range a.checklist {
			if p.state == CandidatePairStateSucceeded && !p.Equal(a.selectedPair) {
				fromNow := a.timeBeforeNow.GetTimeBeforeNow(a.frequentlyTimer)
				if p.lastPongTime.Before(fromNow) {
					p.Attrs().AddLost()
				}
				a.selector.PingCandidate(p)
				hivePair = true
			}
		}
		if !hivePair {
			a.frequentlyCheck = false
			return
		}
		timeout := a.frequentlyTimer
		a.frequentlyTimer = a.frequentlyTimer / 2
		if a.frequentlyTimer <= time.Millisecond*100 {
			a.frequentlyTimer = time.Millisecond * 100
		}
		a.timerWorker.DelayTimerTask(timeout, a.frequentlyCheckKeepalive)
	}
}

func (a *Agent) frequentlyCheckStart() {
	if a.supportRenomination && a.isControlling &&
		a.connectionState == ConnectionStateCompleted &&
		a.selectedPair.lastPongTime.Before(a.selectedPair.lastPingTime) &&
		time.Since(a.selectedPair.lastPongTime) > a.keepaliveInterval*2/3 && !a.frequentlyCheck {
		a.log.Debugf("Renomination-frequently check other pair start")
		a.frequentlyCheck = true
		a.frequentlyTimer = 1 * time.Second
		for _, p := range a.checklist {
			if p.state == CandidatePairStateSucceeded && !p.Equal(a.selectedPair) {
				p.Attrs().ClearPingTimes()
			}
		}
		a.frequentlyCheckKeepalive()
	}
}

// validateSelectedPair checks if the selected pair is (still) valid
// Note: the caller should hold the agent lock.
func (a *Agent) validateSelectedPair() bool {
	selectedPair, err := a.getSelectedPair()
	if err != nil {
		return false
	}

	a.frequentlyCheckStart()
	//如果当前selectedPair失效(心跳保活超时)，则设置对应pair的state=CandidatePairStateFailed，且nominated = false
	if (a.connectionTimeout != 0) &&
		(time.Since(selectedPair.lastPongTime) > a.connectionTimeout) {
		a.log.Debugf("check the selected pair (%s <-> %s) connection timeout",
			a.selectedPair.local, a.selectedPair.remote)
		a.selectedPairMutex.Lock()
		if a.supportRenomination {
			a.selectedPair.state = CandidatePairStateFailed
			a.selectedPair.pingTimes = 0
		}
		a.frequentlyCheck = false
		a.selectedPair.nominated = false
		a.selectedPair = nil
		a.selectedPairMutex.Unlock()
		if !a.supportRenomination {
			a.updateConnectionState(ConnectionStateDisconnected)
		}
		return false
	}

	return true
}

// checkKeepalive sends STUN Binding Indications to the selected pair
// if no packet has been sent on that pair in the last keepaliveInterval
// Note: the caller should hold the agent lock.
func (a *Agent) checkKeepalive() {
	selectedPair, err := a.getSelectedPair()
	if err != nil {
		return
	}

	if (a.keepaliveInterval != 0) &&
		(time.Since(selectedPair.lastPingTime) > a.keepaliveInterval) {
		// we use binding request instead of indication to support refresh consent schemas
		// see https://tools.ietf.org/html/rfc7675
		a.log.Debugf("ping the selectedPair (%s <-> %s)", selectedPair.local, selectedPair.remote)
		a.selector.PingCandidate(selectedPair)
	}
	a.checkWeakKeepalive()
}

// checkWeakKeepalive sends STUN Binding Request for the other weak connection pair
// Note: the caller should hold the agent lock.
//给selectedPair之外的其他weakPair发送keepAlive心跳
func (a *Agent) checkWeakKeepalive() bool {
	if a.supportRenomination && a.weakKeepaliveInterval != 0 {
		if a.connectionState == ConnectionStateDisconnected {
			return false
		}
		failed := true
		for _, p := range a.checklist {

			if time.Since(p.lastPingTime) > a.weakKeepaliveInterval {
				if p.state == CandidatePairStateFailed {
					a.log.Debugf("Renomination-ping failed for %s, waiting it for count %d\n", p, p.bindingRequestCount)
					if p.bindingRequestCount > 0 {
						p.bindingRequestCount--
						continue
					}
				}

				if p.bindingRequestCount > a.maxBindingRequests && time.Since(p.lastPongTime) > a.connectionTimeout {
					a.log.Debugf("Renomination-max requests reached for pair %s, marking it as failed\n", p)
					p.state = CandidatePairStateFailed
					p.pingTimes = 0
				} else {
					a.selector.PingCandidate(p)
					p.bindingRequestCount++
				}
			}
			if p.state != CandidatePairStateFailed {
				failed = false
			}
		}
		if failed {
			a.log.Debugf("Renomination-update agent state disconnected")
			a.updateConnectionState(ConnectionStateDisconnected)
			return false
		}
	}

	return true
}

// AddRemoteCandidate adds a new remote candidate
func (a *Agent) AddRemoteCandidate(c Candidate) error {
	// If we have a mDNS Candidate lets fully resolve it before adding it locally
	if c.Type() == CandidateTypeHost && strings.HasSuffix(c.Address(), ".local") {
		if a.mDNSMode == MulticastDNSModeDisabled {
			a.log.Warnf("remote mDNS candidate added, but mDNS is disabled: (%s)", c.Address())
			return nil
		}

		hostCandidate, ok := c.(*CandidateHost)
		if !ok {
			return ErrAddressParseFailed
		}

		go a.resolveAndAddMulticastCandidate(hostCandidate)
		return nil
	}

	return a.run(func(agent *Agent) {
		agent.addRemoteCandidate(c)
	})
}

func (a *Agent) resolveAndAddMulticastCandidate(c *CandidateHost) {
	_, src, err := a.mDNSConn.Query(context.TODO(), c.Address())
	if err != nil {
		a.log.Warnf("Failed to discover mDNS candidate %s: %v", c.Address(), err)
		return
	}

	ip, _, _, _ := parseAddr(src)
	if ip == nil {
		a.log.Warnf("Failed to discover mDNS candidate %s: failed to parse IP", c.Address())
		return
	}

	if err = c.setIP(ip); err != nil {
		a.log.Warnf("Failed to discover mDNS candidate %s: %v", c.Address(), err)
		return
	}

	if err = a.run(func(agent *Agent) {
		agent.addRemoteCandidate(c)
	}); err != nil {
		a.log.Warnf("Failed to add mDNS candidate %s: %v", c.Address(), err)
		return

	}
}

func (a *Agent) requestConnectivityCheck() {
	//always run in worker thread
	if a.selector != nil {
		a.selector.ContactCandidates()
	}
}

// addRemoteCandidate assumes you are holding the lock (must be execute using a.run)
func (a *Agent) addRemoteCandidate(c Candidate) {
	set := a.remoteCandidates[c.NetworkType()]
	var setNew []Candidate
	for _, candidate := range set {
		if candidate.Equal(c) {
			if c.Priority() > candidate.Priority() {
				//setNew = append(setNew, c)
				continue
			} else if c.Generation() > 0 {
				a.log.Debugf("%s set remote candidate generation:%d", candidate.String(), c.Generation())
				candidate.SetGeneration(c.Generation())
				c = candidate
			}
			continue
		}
		setNew = append(setNew, candidate)
	}
	if c.Generation() > a.maxGeneration {
		a.log.Debugf("%s update max generation:%d", c.String(), c.Generation())
		a.maxGeneration = c.Generation()
	}
	//TODO: limit candidate's count
	setNew = append(setNew, c)
	a.remoteCandidates[c.NetworkType()] = setNew

	if localCandidates, ok := a.localCandidates[c.NetworkType()]; ok {
		for _, localCandidate := range localCandidates {
			if _, ok := localCandidate.(*CandidateReuse); ok {
				d := getDispatcher(localCandidate.addr().IP.String(), uint16(localCandidate.addr().Port))
				err := d.RegisterCand(uint16(localCandidate.addr().Port), localCandidate.addr().IP.String(), localCandidate, c, "", "")
				if err != nil {
					a.log.Error("set remote cand failed")
				}
			}
			a.addPair(localCandidate, c)
		}
	}

	a.requestConnectivityCheck()
}

// addCandidate assumes you are holding the lock (must be execute using a.run)
func (a *Agent) addCandidate(c Candidate) {
	set := a.localCandidates[c.NetworkType()]
	var setNew []Candidate
	for _, candidate := range set {
		if candidate.Equal(c) {
			if c.Priority() > candidate.Priority() {
				setNew = append(setNew, c)
				continue
			} else if c.Generation() > 0 {
				a.log.Debugf("%s set local candidate generation:%d", candidate.String(), c.Generation())
				candidate.SetGeneration(c.Generation())
				c = candidate
			}
			continue
		}
		setNew = append(setNew, candidate)
	}
	if c.Generation() > a.maxGeneration {
		a.log.Debugf("%s update max generation:%d", c.String(), c.Generation())
		a.maxGeneration = c.Generation()
	}
	setNew = append(setNew, c)
	a.localCandidates[c.NetworkType()] = setNew

	a.log.Debugf("Add local candidate: %s", c)
	if remoteCandidates, ok := a.remoteCandidates[c.NetworkType()]; ok {
		for _, remoteCandidate := range remoteCandidates {
			a.addPair(c, remoteCandidate)
		}
	}
}

// GetLocalCandidates returns the local candidates
func (a *Agent) GetLocalCandidates() ([]Candidate, error) {
	res := make(chan []Candidate)

	err := a.run(func(agent *Agent) {
		var candidates []Candidate
		for _, set := range agent.localCandidates {
			candidates = append(candidates, set...)
		}
		res <- candidates
	})
	if err != nil {
		return nil, err
	}

	return <-res, nil
}

func (a *Agent) GetRemoteCandidates() ([]Candidate, error) {
	res := make(chan []Candidate)

	err := a.run(func(agent *Agent) {
		var candidates []Candidate
		for _, set := range agent.remoteCandidates {
			candidates = append(candidates, set...)
		}
		res <- candidates
	})
	if err != nil {
		return nil, err
	}

	return <-res, nil
}

// GetLocalUserCredentials returns the local user credentials
func (a *Agent) GetLocalUserCredentials() (frag string, pwd string) {
	return a.localUfrag, a.localPwd
}

// Close cleans up the Agent
func (a *Agent) Close() error {
	done := make(chan struct{})
	err := a.run(func(agent *Agent) {
		defer func() {
			close(done)
		}()
		agent.err.Store(ErrClosed)
		close(agent.done)

		// Cleanup all candidates
		for net, cs := range agent.localCandidates {
			for _, c := range cs {
				err := c.close()
				if err != nil {
					a.log.Warnf("Failed to close candidate %s: %v", c, err)
				}
			}
			delete(agent.localCandidates, net)
		}
		for net, cs := range agent.remoteCandidates {
			for _, c := range cs {
				err := c.close()
				if err != nil {
					a.log.Warnf("Failed to close candidate %s: %v", c, err)
				}
			}
			delete(agent.remoteCandidates, net)
		}
		a.selectedPairMutex.Lock()
		a.selectedPair = nil
		a.selectedPairMutex.Unlock()

		if err := a.buffer.Close(); err != nil {
			a.log.Warnf("failed to close buffer: %v", err)
		}

		if a.connectivityTicker != nil {
			a.connectivityTicker.Stop()
		}

		a.closeMulticastConn()
	})
	if err != nil {
		return err
	}

	<-done
	a.updateConnectionState(ConnectionStateClosed)

	return nil
}

func (a *Agent) findRemoteCandidate(networkType NetworkType, addr net.Addr) Candidate {
	var ip net.IP
	var port int

	switch casted := addr.(type) {
	case *net.UDPAddr:
		ip = casted.IP
		port = casted.Port
	case *net.TCPAddr:
		ip = casted.IP
		port = casted.Port
	default:
		a.log.Warnf("unsupported address type %T", a)
		return nil
	}

	set := a.remoteCandidates[networkType]
	for _, c := range set {
		if c.addr().IP.Equal(ip) && c.Port() == port {
			return c
		}
	}
	return nil
}

func (a *Agent) sendBindingRequest(m *stun.Message, local, remote Candidate) {
	a.log.Tracef("ping STUN from %s to %s\n", local.String(), remote.String())

	a.invalidatePendingBindingRequests(time.Now())
	a.pendingBindingRequests = append(a.pendingBindingRequests, bindingRequest{
		timestamp:      time.Now(),
		transactionID:  m.TransactionID,
		destination:    remote.addr(),
		isUseCandidate: m.Contains(stun.AttrUseCandidate),
	})

	a.sendSTUN(m, local, remote)
}

func (a *Agent) sendBindingSuccess(m *stun.Message, local, remote Candidate, nomination uint32) {
	base := remote
	if out, err := stun.Build(m, stun.BindingSuccess,
		&stun.XORMappedAddress{
			IP:   base.addr().IP,
			Port: base.addr().Port,
		},
	); err != nil {
		a.log.Warnf("Failed to handle inbound ICE from: %s to: %s error: %s", local, remote, err)
	} else {
		if nomination > 0 {
			NominationAttr{nomination: nomination}.AddTo(out)
		}
		stun.NewShortTermIntegrity(a.localPwd).AddTo(out)
		stun.Fingerprint.AddTo(out)
		a.sendSTUN(out, local, remote)
	}
}

/* Removes pending binding requests that are over maxBindingRequestTimeout old

   Let HTO be the transaction timeout, which SHOULD be 2*RTT if
   RTT is known or 500 ms otherwise.
   https://tools.ietf.org/html/rfc8445#appendix-B.1
*/
func (a *Agent) invalidatePendingBindingRequests(filterTime time.Time) {
	initialSize := len(a.pendingBindingRequests)

	temp := a.pendingBindingRequests[:0]
	for _, bindingRequest := range a.pendingBindingRequests {
		if filterTime.Sub(bindingRequest.timestamp) < maxBindingRequestTimeout {
			temp = append(temp, bindingRequest)
		}
	}

	a.pendingBindingRequests = temp
	if bindRequestsRemoved := initialSize - len(a.pendingBindingRequests); bindRequestsRemoved > 0 {
		a.log.Tracef("Discarded %d binding requests because they expired", bindRequestsRemoved)
	}
}

// Assert that the passed TransactionID is in our pendingBindingRequests and returns the destination
// If the bindingRequest was valid remove it from our pending cache
func (a *Agent) handleInboundBindingSuccess(id [stun.TransactionIDSize]byte) (bool, *bindingRequest) {
	a.invalidatePendingBindingRequests(time.Now())
	for i := range a.pendingBindingRequests {
		if a.pendingBindingRequests[i].transactionID == id {
			validBindingRequest := a.pendingBindingRequests[i]
			a.pendingBindingRequests = append(a.pendingBindingRequests[:i], a.pendingBindingRequests[i+1:]...)
			return true, &validBindingRequest
		}
	}
	return false, nil
}

// handleInbound processes STUN traffic from a remote candidate
func (a *Agent) handleInbound(m *stun.Message, local Candidate, remote net.Addr) {
	var err error
	if m == nil || local == nil {
		return
	}

	if m.Type.Method != stun.MethodBinding ||
		!(m.Type.Class == stun.ClassSuccessResponse ||
			m.Type.Class == stun.ClassRequest ||
			m.Type.Class == stun.ClassIndication) {
		a.log.Tracef("unhandled STUN from %s to %s class(%s) method(%s)", remote, local, m.Type.Class, m.Type.Method)
		return
	}

	if a.isControlling {
		if m.Contains(stun.AttrICEControlling) {
			a.log.Debug("inbound isControlling && a.isControlling == true")
			return
		} else if m.Contains(stun.AttrUseCandidate) {
			a.log.Debug("useCandidate && a.isControlling == true")
			return
		}
	} else {
		if m.Contains(stun.AttrICEControlled) {
			a.log.Debug("inbound isControlled && a.isControlling == false")
			return
		}
	}

	remoteCandidate := a.findRemoteCandidate(local.NetworkType(), remote)
	if m.Type.Class == stun.ClassSuccessResponse {
		if err = assertInboundMessageIntegrity(m, []byte(a.remotePwd)); err != nil {
			a.log.Warnf("discard message from (%s), %v", remote, err)
			a.callback(ReceiveErrorResponse, local.String(), remote.String())
			return
		}

		if remoteCandidate == nil {
			a.log.Warnf("discard success message from (%s), no such remote", remote)
			return
		}
		a.callback(ReceiveSuccessResponse, local.String(), remote.String())
		a.selector.HandleSuccessResponse(m, local, remoteCandidate, remote)
	} else if m.Type.Class == stun.ClassRequest {
		if err = assertInboundUsername(m, a.localUfrag+":"+a.remoteUfrag); err != nil {
			a.log.Warnf("discard message from (%s), %v", remote, err)
			return
		} else if err = assertInboundMessageIntegrity(m, []byte(a.localPwd)); err != nil {
			a.log.Warnf("discard message from (%s), %v", remote, err)
			return
		}

		// the agent may not started when received request.
		if a.selector == nil {
			return
		}
		if remoteCandidate == nil {
			ip, port, networkType, ok := parseAddr(remote)
			if !ok {
				a.log.Errorf("Failed to create parse remote net.Addr when creating remote prflx candidate")
				return
			}

			prflxCandidateConfig := CandidatePeerReflexiveConfig{
				Network:   networkType.String(),
				Address:   ip.String(),
				Port:      port,
				Component: local.Component(),
				RelAddr:   "",
				RelPort:   0,
			}

			prflxCandidate, err := NewCandidatePeerReflexive(&prflxCandidateConfig)
			if err != nil {
				a.log.Errorf("Failed to create new remote prflx candidate (%s)", err)
				return
			}
			remoteCandidate = prflxCandidate

			a.log.Debugf("adding a new peer-reflexive candidate: %s ", remote)
			a.addRemoteCandidate(remoteCandidate)
		}

		a.log.Tracef("inbound STUN (Request) from %s to %s", remote.String(), local.String())
		a.callback(ReceiveRequest, local.String(), remote.String())

		a.selector.HandleBindingRequest(m, local, remoteCandidate)
	}

	if remoteCandidate != nil {
		remoteCandidate.seen(false)
	}
}

// noSTUNSeen processes non STUN traffic from a remote candidate,
// and returns true if it is an actual remote candidate
func (a *Agent) noSTUNSeen(local Candidate, remote net.Addr) bool {
	remoteCandidate := a.findRemoteCandidate(local.NetworkType(), remote)
	if remoteCandidate == nil {
		return false
	}

	remoteCandidate.seen(false)
	return true
}

func (a *Agent) getSelectedPair() (*candidatePair, error) {
	a.selectedPairMutex.RLock()
	selectedPair := a.selectedPair
	a.selectedPairMutex.RUnlock()

	if selectedPair == nil {
		return nil, ErrNoCandidatePairs
	}

	return selectedPair, nil
}

func (a *Agent) closeMulticastConn() {
	if a.mDNSConn != nil {
		if err := a.mDNSConn.Close(); err != nil {
			a.log.Warnf("failed to close mDNS Conn: %v", err)
		}
	}
}

// GetCandidatePairsStats returns a list of candidate pair stats
func (a *Agent) GetCandidatePairsStats() []CandidatePairStats {
	resultChan := make(chan []CandidatePairStats)
	err := a.run(func(agent *Agent) {
		result := make([]CandidatePairStats, 0, len(agent.checklist))
		for _, cp := range agent.checklist {
			stat := CandidatePairStats{
				Timestamp:         time.Now(),
				LocalCandidateID:  cp.local.ID(),
				RemoteCandidateID: cp.remote.ID(),
				State:             cp.state,
				Nominated:         cp.nominated,
				Priority:          cp.priority,
				// PacketsSent uint32
				// PacketsReceived uint32
				// BytesSent uint64
				// BytesReceived uint64
				// LastPacketSentTimestamp time.Time
				// LastPacketReceivedTimestamp time.Time
				// FirstRequestTimestamp time.Time
				LastRequestTimestamp:  cp.lastPingTime,
				LastResponseTimestamp: cp.lastPongTime,
				CandidatePairAttrs:    cp.Attrs(),
				//TotalRoundTripTime:    cp.Attrs().Rtt,
				// CurrentRoundTripTime float64
				// AvailableOutgoingBitrate float64
				// AvailableIncomingBitrate float64
				// CircuitBreakerTriggerCount uint32
				// RequestsReceived uint64
				// RequestsSent uint64
				// ResponsesReceived uint64
				// ResponsesSent uint64
				// RetransmissionsReceived uint64
				// RetransmissionsSent uint64
				// ConsentRequestsSent uint64
				// ConsentExpiredTimestamp time.Time
				CandidateStatsLocal: CandidateStats{
					Timestamp:     time.Now(),
					ID:            cp.local.ID(),
					NetworkType:   cp.local.NetworkType(),
					IP:            cp.local.Address(),
					Port:          cp.local.Port(),
					CandidateType: cp.local.Type(),
					Priority:      cp.local.Priority(),
					Generation:    cp.local.Generation(),
					// URL string
					RelayProtocol: "udp",
					// Deleted bool
				},
				CandidateStatsRemote: CandidateStats{
					Timestamp:     time.Now(),
					ID:            cp.remote.ID(),
					NetworkType:   cp.remote.NetworkType(),
					IP:            cp.remote.Address(),
					Port:          cp.remote.Port(),
					CandidateType: cp.remote.Type(),
					Priority:      cp.remote.Priority(),
					Generation:    cp.remote.Generation(),
					// URL string
					RelayProtocol: "udp",
					// Deleted bool
				},
			}
			result = append(result, stat)
		}
		resultChan <- result
	})
	if err != nil {
		a.log.Errorf("error getting candidate pairs stats %v", err)
		return []CandidatePairStats{}
	}
	return <-resultChan
}

// GetLocalCandidatesStats returns a list of local candidates stats
func (a *Agent) GetLocalCandidatesStats() []CandidateStats {
	resultChan := make(chan []CandidateStats)
	err := a.run(func(agent *Agent) {
		result := make([]CandidateStats, 0, len(agent.localCandidates))
		for networkType, localCandidates := range agent.localCandidates {
			for _, c := range localCandidates {
				stat := CandidateStats{
					Timestamp:     time.Now(),
					ID:            c.ID(),
					NetworkType:   networkType,
					IP:            c.Address(),
					Port:          c.Port(),
					CandidateType: c.Type(),
					Priority:      c.Priority(),
					// URL string
					RelayProtocol: "udp",
					// Deleted bool
				}
				result = append(result, stat)
			}
		}
		resultChan <- result
	})
	if err != nil {
		a.log.Errorf("error getting candidate pairs stats %v", err)
		return []CandidateStats{}
	}
	return <-resultChan
}

// GetRemoteCandidatesStats returns a list of remote candidates stats
func (a *Agent) GetRemoteCandidatesStats() []CandidateStats {
	resultChan := make(chan []CandidateStats)
	err := a.run(func(agent *Agent) {
		result := make([]CandidateStats, 0, len(agent.remoteCandidates))
		for networkType, localCandidates := range agent.remoteCandidates {
			for _, c := range localCandidates {
				stat := CandidateStats{
					Timestamp:     time.Now(),
					ID:            c.ID(),
					NetworkType:   networkType,
					IP:            c.Address(),
					Port:          c.Port(),
					CandidateType: c.Type(),
					Priority:      c.Priority(),
					// URL string
					RelayProtocol: "udp",
				}
				result = append(result, stat)
			}
		}
		resultChan <- result
	})
	if err != nil {
		a.log.Errorf("error getting candidate pairs stats %v", err)
		return []CandidateStats{}
	}
	return <-resultChan
}

func (a *Agent) callback(event Event, local string, remote string) {
	if a.onEventHdlr != nil {
		a.onEventHdlr(event, local, remote)
	}
}

func (a *Agent) SetRemoteCredentials(username string, password string) {
	a.run(func(agent *Agent) {
		agent.remoteUfrag = username
		agent.remotePwd = password
		agent.requestConnectivityCheck()
		agent.pacedCheck(20, 0)
	})
}

func (a *Agent) SetDataHandler(h DataHandler) {
	a.dataHandler = h
}

// Role represents ICE agent role, which can be controlling or controlled.
type Role byte

// UnmarshalText implements TextUnmarshaler.
func (r *Role) UnmarshalText(text []byte) error {
	switch string(text) {
	case "controlling":
		*r = Controlling
	case "controlled":
		*r = Controlled
	default:
		return fmt.Errorf("unknown role %q", text)
	}
	return nil
}

// MarshalText implements TextMarshaler.
func (r Role) MarshalText() (text []byte, err error) {
	return []byte(r.String()), nil
}

func (r Role) String() string {
	switch r {
	case Controlling:
		return "controlling"
	case Controlled:
		return "controlled"
	default:
		return "unknown"
	}
}

// Possible ICE agent roles.
const (
	Controlling Role = iota
	Controlled
)
