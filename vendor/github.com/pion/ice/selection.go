package ice

import (
	"net"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun"
)

type pairCandidateSelector interface {
	Start()
	ContactCandidates()
	PingCandidate(local, remote Candidate)
	HandleSuccessResponse(m *stun.Message, local, remote Candidate, remoteAddr net.Addr)
	HandleBindingRequest(m *stun.Message, local, remote Candidate)
}

type controllingSelector struct {
	startTime              time.Time
	agent                  *Agent
	nominatedPair          *candidatePair
	nominationRequestCount uint16
	log                    logging.LeveledLogger
}
//该函数会在agent执行结束或者执行超时后才会执行
func (s *controllingSelector) Start() {
	s.startTime = time.Now()
	go func() {
		select {
		case <-s.agent.done:
			return
		case <-time.After(s.agent.candidateSelectionTimeout):
		}

		err := s.agent.run(func(a *Agent) {
			if s.nominatedPair == nil {
				p := s.agent.getBestValidCandidatePair()
				if p == nil {
					s.log.Trace("check timeout reached and no valid candidate pair found, marking connection as failed")
					s.agent.updateConnectionState(ConnectionStateFailed)
				} else {
					s.log.Tracef("check timeout reached, nominating (%s, %s)", p.local.String(), p.remote.String())
					s.nominatedPair = p
					s.nominatePair(p)
				}
			}
		})

		if err != nil {
			s.log.Errorf("error processing checkCandidatesTimeout handler %v", err.Error())
		}
	}()
}

func (s *controllingSelector) isNominatable(c Candidate) bool {
	switch {
	case c.Type() == CandidateTypeHost:
		return time.Since(s.startTime).Nanoseconds() > s.agent.hostAcceptanceMinWait.Nanoseconds()
	case c.Type() == CandidateTypeServerReflexive:
		return time.Since(s.startTime).Nanoseconds() > s.agent.srflxAcceptanceMinWait.Nanoseconds()
	case c.Type() == CandidateTypePeerReflexive:
		return time.Since(s.startTime).Nanoseconds() > s.agent.prflxAcceptanceMinWait.Nanoseconds()
	case c.Type() == CandidateTypeRelay:
		return time.Since(s.startTime).Nanoseconds() > s.agent.relayAcceptanceMinWait.Nanoseconds()
	}

	s.log.Errorf("isNominatable invalid candidate type %s", c.Type().String())
	return false
}

func (s *controllingSelector) ContactCandidates() {
	switch {
	case s.agent.getSelectedPair() != nil:
		//对selectPair进行心跳保活
		if s.agent.validateSelectedPair() {
			s.log.Trace("checking keepalive")
			s.agent.checkKeepalive()
		}
	case s.nominatedPair != nil:
		//在nominatePair上持续的发送Binding request，直至达到上限或者一致有效
		if s.nominationRequestCount > s.agent.maxBindingRequests {
			s.log.Trace("max nomination requests reached, setting the connection state to failed")
			s.agent.updateConnectionState(ConnectionStateFailed)
			return
		}
		//提名方式又分为普通提名和进取型提名
		//普通提名方式会做两次连通性检查，在第一次做连通性检查时不会带上USE-CANDIDATE属性，而是在生成的validlist里选择pair再进行一次连通性检查，这时会带上USE-CANDIDATE属性，并且置位nominated flag。
		//进取型方式则是每次发送连通性检查时都会带上USE-CANDIDATE属性，并且置位nominated flag，不会再去做第二次连通性检查。
		s.nominatePair(s.nominatedPair)
	default:
		//对checklist进行连通性，此处会对checklist进行优先级排序，选择优先级最高的进行提名
		p := s.agent.getBestValidCandidatePair()
		if p != nil && s.isNominatable(p.local) && s.isNominatable(p.remote) {
			s.log.Tracef("Nominatable pair found, nominating (%s, %s)", p.local.String(), p.remote.String())
			p.nominated = true
			s.nominatedPair = p
			//提名其实就是发送Binding request的过程
			s.nominatePair(p)
			return
		}
		//给checklist中的所有pair发送Binding request
		//如果ping次数超出了上限，则将对应的pair置为CandidatePairStateFailed
		s.agent.pingAllCandidates()
	}
}

func (s *controllingSelector) nominatePair(pair *candidatePair) {
	// The controlling agent MUST include the USE-CANDIDATE attribute in
	// order to nominate a candidate pair (Section 8.1.1).  The controlled
	// agent MUST NOT include the USE-CANDIDATE attribute in a Binding
	// request.
	msg, err := stun.Build(stun.BindingRequest, stun.TransactionID,
		stun.NewUsername(s.agent.remoteUfrag+":"+s.agent.localUfrag),
		UseCandidate,
		AttrControlling(s.agent.tieBreaker),
		PriorityAttr(pair.local.Priority()),
		stun.NewShortTermIntegrity(s.agent.remotePwd),
		stun.Fingerprint,
	)

	if err != nil {
		s.log.Error(err.Error())
		return
	}

	s.log.Tracef("ping STUN (nominate candidate pair) from %s to %s\n", pair.local.String(), pair.remote.String())
	s.agent.sendBindingRequest(msg, pair.local, pair.remote)
	s.nominationRequestCount++
}

//controlling处理controled BindingRequest
/*
	何时会收到controled端Request
	controled端发送BindingRequest到controlling端
	controlling端收到后，判断该Pair是否为CandidatePairStateSucceeded
	如果是，恰好是优先级最高的Pair，并且当前还没正在提名的Pair
	则对该Pair进行提名操作
*/
//收到controled端Request后，首先返回一个BindingSuccess，表示请求成功
//如果该Pair状态为Succeeded，说明之前该pair已经Ping通
//如果该pair状态为Succeeded，但是还没有正在提名的pair，也没成功提名的pair
//检查该pair是否为chekelist中优先级最高的pair，如果是，则设置该pair为待提名pair，并进行提名操作
func (s *controllingSelector) HandleBindingRequest(m *stun.Message, local, remote Candidate) {
	s.agent.sendBindingSuccess(m, local, remote)
	//如果checklist中还未保存该pair，则增加该pair
	p := s.agent.findPair(local, remote)

	if p == nil {
		s.agent.addPair(local, remote)
		return
	}
	//如果该pair状态为Succeeded，但是还没有正在提名的pair，也没成功提名的pair
	//检查该pair是否为chekelist中优先级最高的pair，如果是，则设置该pair为待提名pair，并进行提名操作
	if p.state == CandidatePairStateSucceeded && s.nominatedPair == nil && s.agent.getSelectedPair() == nil {
		bestPair := s.agent.getBestAvailableCandidatePair()
		if bestPair == nil {
			s.log.Tracef("No best pair available\n")
		} else if bestPair.Equal(p) && s.isNominatable(p.local) && s.isNominatable(p.remote) {
			s.log.Tracef("The candidate (%s, %s) is the best candidate available, marking it as nominated\n",
				p.local.String(), p.remote.String())
			s.nominatedPair = p
			s.nominatePair(p)
		}
	}
}

//controlling处理controled BindingSuccessResponse
/*
	何时会收到controled端Response
	当controlling端在某Pair上发送发送BindingRequest时，controled会回复一个BindingSuccessResponse
	controlling端收到BindingSuccessResponse后，该pair状态变为CandidatePairStateSucceeded
	如果Response对应的Request请求是提名操作，即Binding中带UseCandidate标记
	则收到该Response说明提名成功
	此时该pair成为Selected Pair
*/
//check返回的BindingResponse是否超时
//如果未超时，判断其ID是否在pendingBinding中
//如果是，判断BindingResponse包的地址与remoteAddr是否匹配
//如果匹配，查看是否是Candidate Pair
//设置该Pair状态为CandidatePairStateSucceeded
//如果是，查看pendingRequest是否为UseCandidate(提名的标志)
//如果是，说明提名成功，将当前Pair设置为Selected Pair
func (s *controllingSelector) HandleSuccessResponse(m *stun.Message, local, remote Candidate, remoteAddr net.Addr) {
	ok, pendingRequest := s.agent.handleInboundBindingSuccess(m.TransactionID)
	if !ok {
		s.log.Warnf("discard message from (%s), unknown TransactionID 0x%x", remote, m.TransactionID)
		return
	}

	transactionAddr := pendingRequest.destination

	// Assert that NAT is not symmetric
	// https://tools.ietf.org/html/rfc8445#section-7.2.5.2.1
	if !addrEqual(transactionAddr, remoteAddr) {
		s.log.Debugf("discard message: transaction source and destination does not match expected(%s), actual(%s)", transactionAddr, remote)
		return
	}

	s.log.Tracef("inbound STUN (SuccessResponse) from %s to %s", remote.String(), local.String())
	//从checklist中找到对应的(local, remote) pair
	p := s.agent.findPair(local, remote)

	if p == nil {
		// This shouldn't happen
		s.log.Error("Success response from invalid candidate pair")
		return
	}
	//将该pair状态置为Succeeded，并设置为SelectedPair
	p.state = CandidatePairStateSucceeded
	s.log.Tracef("Found valid candidate pair: %s", p)
	//isUseCandidate作为提名请求的标志，表示某对CandidatePair提名成功
	//如果此时SelectedPair为空，则将此次提名成功的CandidatePair作为Selected Pair
	if pendingRequest.isUseCandidate && s.agent.getSelectedPair() == nil {
		s.agent.setSelectedPair(p)
	}
}

func (s *controllingSelector) PingCandidate(local, remote Candidate) {
	msg, err := stun.Build(stun.BindingRequest, stun.TransactionID,
		stun.NewUsername(s.agent.remoteUfrag+":"+s.agent.localUfrag),
		AttrControlling(s.agent.tieBreaker),
		PriorityAttr(local.Priority()),
		stun.NewShortTermIntegrity(s.agent.remotePwd),
		stun.Fingerprint,
	)

	if err != nil {
		s.log.Error(err.Error())
		return
	}

	s.agent.sendBindingRequest(msg, local, remote)
}

type controlledSelector struct {
	startTime time.Time
	agent     *Agent
	log       logging.LeveledLogger
}

func (s *controlledSelector) Start() {
	s.startTime = time.Now()
}

func (s *controlledSelector) ContactCandidates() {
	if s.agent.getSelectedPair() != nil {
		if s.agent.validateSelectedPair() {
			s.log.Trace("checking keepalive")
			s.agent.checkKeepalive()
		}
	} else {
		if time.Since(s.startTime) > s.agent.candidateSelectionTimeout {
			s.log.Trace("check timeout reached and no valid candidate pair found, marking connection as failed")
			s.agent.updateConnectionState(ConnectionStateFailed)
		} else {
			s.agent.pingAllCandidates()
		}
	}
}

func (s *controlledSelector) PingCandidate(local, remote Candidate) {
	msg, err := stun.Build(stun.BindingRequest, stun.TransactionID,
		stun.NewUsername(s.agent.remoteUfrag+":"+s.agent.localUfrag),
		AttrControlled(s.agent.tieBreaker),
		PriorityAttr(local.Priority()),
		stun.NewShortTermIntegrity(s.agent.remotePwd),
		stun.Fingerprint,
	)

	if err != nil {
		s.log.Error(err.Error())
		return
	}

	s.agent.sendBindingRequest(msg, local, remote)
}

//controlled端发送BindingRequest到controlling，并且收到controlling端的SuccessResponse，说明Binding成功
//设置Pair状态为CandidatePairStateSucceeded
func (s *controlledSelector) HandleSuccessResponse(m *stun.Message, local, remote Candidate, remoteAddr net.Addr) {
	// TODO according to the standard we should specifically answer a failed nomination:
	// https://tools.ietf.org/html/rfc8445#section-7.3.1.5
	// If the controlled agent does not accept the request from the
	// controlling agent, the controlled agent MUST reject the nomination
	// request with an appropriate error code response (e.g., 400)
	// [RFC5389].

	ok, pendingRequest := s.agent.handleInboundBindingSuccess(m.TransactionID)
	if !ok {
		s.log.Warnf("discard message from (%s), unknown TransactionID 0x%x", remote, m.TransactionID)
		return
	}

	transactionAddr := pendingRequest.destination

	// Assert that NAT is not symmetric
	// https://tools.ietf.org/html/rfc8445#section-7.2.5.2.1
	if !addrEqual(transactionAddr, remoteAddr) {
		s.log.Debugf("discard message: transaction source and destination does not match expected(%s), actual(%s)", transactionAddr, remote)
		return
	}

	s.log.Tracef("inbound STUN (SuccessResponse) from %s to %s", remote.String(), local.String())

	p := s.agent.findPair(local, remote)
	if p == nil {
		// This shouldn't happen
		s.log.Error("Success response from invalid candidate pair")
		return
	}

	p.state = CandidatePairStateSucceeded
	s.log.Tracef("Found valid candidate pair: %s", p)
}

//controlled端处理controlling端 BindingRequest
//如果不在checklist中，新增到checklist中
//如果是提名请求(useCandidate = true)
	//如果该pair之前状态就是成功的，则将该pair设置为Selected Pair(controlled Ping后收到SuccessResponse即为成功)
	//如果该pair之前状态未成功，仅返回PingRequest作为测通
func (s *controlledSelector) HandleBindingRequest(m *stun.Message, local, remote Candidate) {
	useCandidate := m.Contains(stun.AttrUseCandidate)

	p := s.agent.findPair(local, remote)

	if p == nil {
		p = s.agent.addPair(local, remote)
	}

	if useCandidate {
		// https://tools.ietf.org/html/rfc8445#section-7.3.1.5

		if p.state == CandidatePairStateSucceeded {
			// If the state of this pair is Succeeded, it means that the check
			// previously sent by this pair produced a successful response and
			// generated a valid pair (Section 7.2.5.3.2).  The agent sets the
			// nominated flag value of the valid pair to true.
			//如果Pair之前状态成功，并且收到提名，则将该Pair设置为Selected Pair
			if selectedPair := s.agent.getSelectedPair(); selectedPair == nil {
				s.agent.setSelectedPair(p)
			}
			//回复一个BindingSuccessResponse
			s.agent.sendBindingSuccess(m, local, remote)
		} else {
			// If the received Binding request triggered a new check to be
			// enqueued in the triggered-check queue (Section 7.3.1.4), once the
			// check is sent and if it generates a successful response, and
			// generates a valid pair, the agent sets the nominated flag of the
			// pair to true.  If the request fails (Section 7.2.5.2), the agent
			// MUST remove the candidate pair from the valid list, set the
			// candidate pair state to Failed, and set the checklist state to
			// Failed.
			//之前Pair状态未成功，仅返回PingRequest
			s.PingCandidate(local, remote)
		}
	} else {
		//如果不是提名操作，同时返回BindingSuccess和Ping
		s.agent.sendBindingSuccess(m, local, remote)
		s.PingCandidate(local, remote)
	}
}

type liteSelector struct {
	pairCandidateSelector
}

// A lite selector should not contact candidates
func (s *liteSelector) ContactCandidates() {
	if _, ok := s.pairCandidateSelector.(*controllingSelector); ok {
		// pion/ice#96
		// TODO: implement lite controlling agent. For now falling back to full agent.
		// This only happens if both peers are lite. See RFC 8445 S6.1.1 and S6.2
		s.pairCandidateSelector.ContactCandidates()
	}
}