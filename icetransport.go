// +build !js

package webrtc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v2/internal/mux"
)

// ICETransport allows an application access to information about the ICE
// transport over which packets are sent and received.
type ICETransport struct {
	lock sync.RWMutex

	role ICERole
	// Component ICEComponent
	// State ICETransportState
	// gatheringState ICEGathererState

	onConnectionStateChangeHdlr       atomic.Value // func(ICETransportState)
	onSelectedCandidatePairChangeHdlr atomic.Value // func(*ICECandidatePair)

	state ICETransportState

	gatherer *ICEGatherer
	conn     *ice.Conn
	mux      *mux.Mux

	loggerFactory logging.LoggerFactory

	log logging.LeveledLogger
}

// func (t *ICETransport) GetLocalCandidates() []ICECandidate {
//
// }
//
// func (t *ICETransport) GetRemoteCandidates() []ICECandidate {
//
// }
//
// func (t *ICETransport) GetSelectedCandidatePair() ICECandidatePair {
//
// }
//
// func (t *ICETransport) GetLocalParameters() ICEParameters {
//
// }
//
// func (t *ICETransport) GetRemoteParameters() ICEParameters {
//
// }

// NewICETransport creates a new NewICETransport.
func NewICETransport(gatherer *ICEGatherer, loggerFactory logging.LoggerFactory) *ICETransport {
	return &ICETransport{
		gatherer:      gatherer,
		loggerFactory: loggerFactory,
		log:           loggerFactory.NewLogger("ortc"),
		state:         ICETransportStateNew,
	}
}

// Start incoming connectivity checks based on its configured role.
/*
ICETransport主要通过代理完成其工作，主要工作包括以下几点:
1、搜集Local candidate，并与remote candidate组成pair，加入到checklist中
2、为checklist中的每个pair对应的local candidate启动一个recv监听，监听该连接上的stun ping
3、之后立马启动连通性测试，在每个pair上发送ping，测试对方是否回复
4、如果对方回复，则证明该连接是成功的，进而开始提名操作，携带UseCandidate属性
5、如果对方成功回复提名，则提名成功，将该pair加入到selectPair中
6、对于selectPair，需要定时保活，如果保活失败，则重复操作stun ping -> 提名 -> selectPair
7、ICE连接时，一方为controlling，另一方为controlled，controlling为主动发起方
8、controlling端调用agent.Dial主动发起，controlled端调用agent.Accept被动接收
9、两者最后都调用ContactCandidates函数，只不过实现不同
 */
func (t *ICETransport) Start(gatherer *ICEGatherer, params ICEParameters, role *ICERole) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	if gatherer != nil {
		t.gatherer = gatherer
	}

	if err := t.ensureGatherer(); err != nil {
		return err
	}

	agent := t.gatherer.getAgent()
	if agent == nil {
		return errors.New("ICEAgent does not exist, unable to start ICETransport")
	}

	if err := agent.OnConnectionStateChange(func(iceState ice.ConnectionState) {
		state := newICETransportStateFromICE(iceState)
		t.lock.Lock()
		t.state = state
		t.lock.Unlock()

		t.onConnectionStateChange(state)
	}); err != nil {
		return err
	}
	if err := agent.OnSelectedCandidatePairChange(func(local, remote ice.Candidate) {
		candidates, err := newICECandidatesFromICE([]ice.Candidate{local, remote})
		if err != nil {
			t.log.Warnf("Unable to convert ICE candidates to ICECandidates: %s", err)
			return
		}
		t.onSelectedCandidatePairChange(NewICECandidatePair(&candidates[0], &candidates[1]))
	}); err != nil {
		return err
	}

	if role == nil {
		controlled := ICERoleControlled
		role = &controlled
	}
	t.role = *role

	// Drop the lock here to allow trickle-ICE candidates to be
	// added so that the agent can complete a connection
	t.lock.Unlock()

	var iceConn *ice.Conn
	var err error
	//这里会进行连通性测试
	//如果是ICERoleControlling方，主动发起连接
	//如果是ICERoleControlled方，接收对方连接请求
	//重点函数入口!!!
	switch *role {
	case ICERoleControlling:
		iceConn, err = agent.Dial(context.TODO(),
			params.UsernameFragment,
			params.Password)

	case ICERoleControlled:
		iceConn, err = agent.Accept(context.TODO(),
			params.UsernameFragment,
			params.Password)

	default:
		err = errors.New("unknown ICE Role")
	}

	// Reacquire the lock to set the connection/mux
	t.lock.Lock()
	if err != nil {
		return err
	}
	//最终可用的conn
	t.conn = iceConn

	config := mux.Config{
		Conn:          t.conn,
		BufferSize:    receiveMTU,
		LoggerFactory: t.loggerFactory,
	}
	//mux作为最终底层传输对象
	//TODO: Multiplexing为多路复用器，RTP/RTCP/DTLS/STUN/TURN/ZRTP都将复用同一个sockct连接
	t.mux = mux.NewMux(config)

	return nil
}

// Stop irreversibly stops the ICETransport.
func (t *ICETransport) Stop() error {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.mux != nil {
		return t.mux.Close()
	} else if t.gatherer != nil {
		return t.gatherer.Close()
	}
	return nil
}

// OnSelectedCandidatePairChange sets a handler that is invoked when a new
// ICE candidate pair is selected
func (t *ICETransport) OnSelectedCandidatePairChange(f func(*ICECandidatePair)) {
	t.onSelectedCandidatePairChangeHdlr.Store(f)
}

func (t *ICETransport) onSelectedCandidatePairChange(pair *ICECandidatePair) {
	hdlr := t.onSelectedCandidatePairChangeHdlr.Load()
	if hdlr != nil {
		hdlr.(func(*ICECandidatePair))(pair)
	}
}

// OnConnectionStateChange sets a handler that is fired when the ICE
// connection state changes.
func (t *ICETransport) OnConnectionStateChange(f func(ICETransportState)) {
	t.onConnectionStateChangeHdlr.Store(f)
}

func (t *ICETransport) onConnectionStateChange(state ICETransportState) {
	hdlr := t.onConnectionStateChangeHdlr.Load()
	if hdlr != nil {
		hdlr.(func(ICETransportState))(state)
	}
}

// Role indicates the current role of the ICE transport.
func (t *ICETransport) Role() ICERole {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.role
}

// SetRemoteCandidates sets the sequence of candidates associated with the remote ICETransport.
func (t *ICETransport) SetRemoteCandidates(remoteCandidates []ICECandidate) error {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if err := t.ensureGatherer(); err != nil {
		return err
	}

	agent := t.gatherer.getAgent()
	if agent == nil {
		return errors.New("ICEAgent does not exist, unable to set remote candidates")
	}

	for _, c := range remoteCandidates {
		i, err := c.toICE()
		if err != nil {
			return err
		}
		err = agent.AddRemoteCandidate(i)
		if err != nil {
			return err
		}
	}

	return nil
}

// AddRemoteCandidate adds a candidate associated with the remote ICETransport.
func (t *ICETransport) AddRemoteCandidate(remoteCandidate ICECandidate) error {
	t.lock.RLock()
	defer t.lock.RUnlock()
	//确认Gatherer Agent是否存在，如果不存在，则新建一个
	//agent用于搜集Local candidate
	if err := t.ensureGatherer(); err != nil {
		return err
	}
	//根据remoteCandidate类型生成不同的ICE Candidate结构体
	//CandidateHost、CandidateServerReflexive、CandidatePeerReflexive、CandidateRelay
	c, err := remoteCandidate.toICE()
	if err != nil {
		return err
	}
	//agent模块实际存储了已经获取到的Local candidate
	agent := t.gatherer.getAgent()
	if agent == nil {
		return errors.New("ICEAgent does not exist, unable to add remote candidates")
	}
	//根据网络类型存储remoteCandidate到agent模块，如果已经存在，则跳过
	//将该remoteCandidate与其类型相同的localCandidate组成新的pair对，加入到后续的连通性测试
	err = agent.AddRemoteCandidate(c)
	if err != nil {
		return err
	}

	return nil
}

// State returns the current ice transport state.
func (t *ICETransport) State() ICETransportState {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.state
}

// NewEndpoint registers a new endpoint on the underlying mux.
//在底层mux之上注册一个新的Endpoint
func (t *ICETransport) NewEndpoint(f mux.MatchFunc) *mux.Endpoint {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.mux.NewEndpoint(f)
}

func (t *ICETransport) ensureGatherer() error {
	if t.gatherer == nil {
		return errors.New("gatherer not started")
	} else if t.gatherer.getAgent() == nil && t.gatherer.api.settingEngine.candidates.ICETrickle {
		//Trickle: ???
		// Special case for trickle=true. (issue-707)
		if err := t.gatherer.createAgent(); err != nil {
			return err
		}
	}

	return nil
}

func (t *ICETransport) collectStats(collector *statsReportCollector) {
	t.lock.Lock()
	conn := t.conn
	t.lock.Unlock()

	collector.Collecting()

	stats := TransportStats{
		Timestamp: statsTimestampFrom(time.Now()),
		Type:      StatsTypeTransport,
		ID:        "iceTransport",
	}

	if conn != nil {
		stats.BytesSent = conn.BytesSent()
		stats.BytesReceived = conn.BytesReceived()
	}

	collector.Collect(stats.ID, stats)
}
