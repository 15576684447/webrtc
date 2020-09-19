package ice_support_remonination

import (
	"encoding/binary"
	"net"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun"
)

type pairCandidateSelector interface {
	Start()
	ContactCandidates()
	PingCandidate(p *candidatePair)
	HandleSuccessResponse(m *stun.Message, local, remote Candidate, remoteAddr net.Addr)
	HandleBindingRequest(m *stun.Message, local, remote Candidate)
}

type controllingSelector struct {
	startTime              time.Time
	agent                  *Agent
	nominatedPair          *candidatePair
	nominationRequestCount uint16
	nomination             uint32 //每次选定selectedPair，都会nomination++，然后发送的ping包携带NominationAttr属性
	ackNomination          uint32 //发送的携带NominationAttr属性的ping包返回后，更新ackNomination值
	log                    logging.LeveledLogger
}

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
	case s.agent.selectedPair != nil:
		if s.agent.validateSelectedPair() {
			s.log.Debugf("checking keepalive pair (%s <-> %s)", s.agent.selectedPair.local, s.agent.selectedPair.remote)
			s.agent.checkKeepalive()
		} else if s.agent.supportRenomination && s.agent.checkWeakKeepalive() {
			//如果selectedPair失效，获取当前bestPair进行重提名
			p := s.agent.getBestValidCandidatePair()
			if p != nil && s.isNominatable(p.local) && s.isNominatable(p.remote) {
				s.log.Debugf("Renomination-nominatable pair found, nominating (%s, %s)", p.local.String(), p.remote.String())
				p.nominated = true //该pair开启重提名开关
				s.agent.setSelectedPair(SelectedDisconnectedReason, p)
				s.nomination++ //需要重提名
				s.PingCandidate(p)//立即重提名，ping包携带NominationAttr
				return
			}
			if s.agent.checkConnectionState() {
				s.log.Debugf("Renomination-change agent state:%d", s.agent.connectionState)
			}
		}
	case s.nominatedPair != nil:
		if s.nominationRequestCount > s.agent.maxBindingRequests {
			s.log.Trace("max nomination requests reached, setting the connection state to failed")
			s.agent.updateConnectionState(ConnectionStateFailed)
			return
		}
		s.nominatePair(s.nominatedPair)
	default:
		p := s.agent.getBestValidCandidatePair()
		if p != nil && s.isNominatable(p.local) && s.isNominatable(p.remote) {
			s.log.Tracef("Nominatable pair found, nominating (%s, %s)", p.local.String(), p.remote.String())
			p.nominated = true
			s.nominatedPair = p
			s.nominatePair(p)
			return
		}

		s.log.Trace("pinging all candidates")
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

func (s *controllingSelector) HandleBindingRequest(m *stun.Message, local, remote Candidate) {
	s.agent.sendBindingSuccess(m, local, remote, 0)

	p := s.agent.findPair(local, remote)

	if p == nil {
		s.agent.addPair(local, remote)
		return
	}

	if p.state == CandidatePairStateSucceeded && s.nominatedPair == nil {
		bestPair := s.agent.getBestValidCandidatePair()
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
	p := s.agent.findPair(local, remote)

	if p == nil {
		// This shouldn't happen
		s.log.Error("Success response from invalid candidate pair")
		return
	}

	p.state = CandidatePairStateSucceeded
	//计算RTT
	rtt := time.Since(pendingRequest.timestamp)
	p.AddRtt(rtt)
	//记录本次ping返回pong的时刻
	p.lastPongTime = time.Now()
	//将该pair的bindingRequestCount清空!!!
	p.bindingRequestCount = 0
	//ping成功次数累加，作为重提名的一项参考依据
	p.pingTimes++
	s.log.Debugf("Found valid candidate pair: %s, rtt:%d", p, rtt)
	//若支持重提名
	if s.agent.supportRenomination {
		//如果selectedPair为空，将该pair设置为selectedPair，nomination++，并立即启动重提名
		if s.agent.selectedPair == nil {
			s.log.Debugf("Renomination-trigger nominate for pair: %s", p)
			s.agent.setSelectedPair(FirstConnectedReason, p)
			s.nomination++ //需要重提名
			s.PingCandidate(p)//立即启动重提名，携带NominationAttr
		} else {//若selectedPair不为空
			//pong包含AttrNomination属性，即重提名包返回，更新ackNomination
			if bin, err := m.Get(AttrNomination); err == nil {
				nomination := binary.BigEndian.Uint32(bin)
				//取出response包中的AttrNomination属性值，即为ackNomination
				//如果当前的resp的nomination值大于记录的ackNomination，设置ackNomination = nomination
				if nomination > s.ackNomination {
					s.log.Debugf("Renomination-Trigger renomination success: %s", p.String())
					s.ackNomination = nomination
				}
			} else {//pong不包含AttrNomination属性，选择更高优先级的pair进行重提名
				//TODO: 解析算法，核心点
				best := s.agent.getBestValidCandidatePair()
				// the connected or completed state may be trigger renominate
				//TODO:如果bestPair优于selectedPair，则进行重提名
				if (s.agent.connectionState == ConnectionStateConnected ||
					(s.agent.connectionState == ConnectionStateCompleted &&
						time.Since(s.agent.lastSelectedPair) > s.agent.renominationMinInterval) &&
						best.pingTimes > 3) && !s.agent.selectedPair.Equal(best) {
					s.log.Debugf("Renomination-trigger the best pair as renomination: %s", p.String())
					s.agent.setSelectedPair(HighPriorityConnectedReason, best)
					s.nomination++//需要重提名
					s.PingCandidate(best)//立即启动重提名，携带NominationAttr
				} else if s.agent.selectedPair.Equal(best) && s.agent.frequentlyCheck {
					//当前bestPair依然为selectedPair，无需重提名
					s.log.Debugf("Renomination-recv selected pair pong, clear frequently check")
					s.agent.frequentlyCheck = false
				}
			}
		}
	} else if pendingRequest.isUseCandidate && s.agent.selectedPair == nil {
		//如果不支持重提名，则依然原始逻辑，pong包携带useCandidate属性才算提名成功
		s.agent.setSelectedPair(FirstConnectedReason, p)
	}
	s.agent.checkConnectionState()
}

func (s *controllingSelector) PingCandidate(p *candidatePair) {
	if s.agent.remoteUfrag == "" || s.agent.remotePwd == "" {
		return
	}
	/*
	msg, err := stun.Build(stun.BindingRequest, stun.TransactionID,
		stun.NewUsername(s.agent.remoteUfrag+":"+s.agent.localUfrag),
		AttrControlling(s.agent.tieBreaker),
		PriorityAttr(local.Priority()),
		stun.NewShortTermIntegrity(s.agent.remotePwd),
		stun.Fingerprint,
	)
	 */
	msg, err := stun.Build(stun.BindingRequest, stun.TransactionID,
		stun.NewUsername(s.agent.remoteUfrag+":"+s.agent.localUfrag),
		AttrControlling(s.agent.tieBreaker),
	)
	if err != nil {
		s.log.Error(err.Error())
		return
	}
	//此处修改了controlling端的ping属性
	//如果支持重提名，提名的pair携带nomination属性；否则正常ping
	//如果不支持重提名，直接采用激进方式，在每个ping中直接携带useCandidate属性
	if s.agent.supportRenomination {
		if p.nominated {
			//重提名方式：当选中selectedPair或者有更高优先级的bestPair，就会nominated=true,nomination++,并立即启动重提名
			//所以如果nomination=ackNomination，则说明不需要重提名
			//每次需要重提名，就会nomination++
			//重提名结束后，收到重提名response，则需要更新ackNomination为最新nomination值
			//此处的秘诀在于：如果重提名一直失败，则携带重提名属性的ping会一直继续，直到重提名成功或者bindingRequestCount超出限制而失败
			if s.nomination > s.ackNomination {
				NominationAttr{nomination: s.nomination}.AddTo(msg)
				s.log.Debugf("Renomination-Ping nominate for %s -> %s", p.local, p.remote)
			}
		}
	} else {
		UseCandidate.AddTo(msg)
	}
	PriorityAttr(p.local.Priority()).AddTo(msg)
	stun.NewShortTermIntegrity(s.agent.remotePwd).AddTo(msg)
	stun.Fingerprint.AddTo(msg)

	p.lastPingTime = time.Now()
	s.agent.sendBindingRequest(msg, p.local, p.remote)
}

type controlledSelector struct {
	startTime        time.Time
	agent            *Agent
	remoteNomination uint32 //记录remote peer端发送的ping包中携带的最新nomination值
	log              logging.LeveledLogger
}

func (s *controlledSelector) Start() {
	s.startTime = time.Now()
	s.remoteNomination = 0
}

func (s *controlledSelector) ContactCandidates() {
	if s.agent.selectedPair != nil {
		if s.agent.validateSelectedPair() {
			s.log.Trace("checking keepalive")
			s.agent.checkKeepalive()
		}
	} else {
		if time.Since(s.startTime) > s.agent.candidateSelectionTimeout {
			s.log.Trace("check timeout reached and no valid candidate pair found, marking connection as failed")
			s.agent.updateConnectionState(ConnectionStateFailed)
		} else {
			s.log.Trace("pinging all candidates")
			s.agent.pingAllCandidates()
		}
	}

	select {
	case <-s.agent.forceCandidateContact:
	default:
	}

}

func (s *controlledSelector) PingCandidate(p *candidatePair) {
	if s.agent.remoteUfrag == "" || s.agent.remotePwd == "" {
		return
	}
	msg, err := stun.Build(stun.BindingRequest, stun.TransactionID,
		stun.NewUsername(s.agent.remoteUfrag+":"+s.agent.localUfrag),
		AttrControlled(s.agent.tieBreaker),
		PriorityAttr(p.local.Priority()),
		stun.NewShortTermIntegrity(s.agent.remotePwd),
		stun.Fingerprint,
	)

	if err != nil {
		s.log.Error(err.Error())
		return
	}
	//记录本次ping的时刻
	p.lastPingTime = time.Now()
	s.agent.sendBindingRequest(msg, p.local, p.remote)
}

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
	//根据ping/pong时间差，计算RTT
	p.AddRtt(time.Since(pendingRequest.timestamp))
	//记录本次ping的返回pong的时刻
	p.lastPongTime = time.Now()
	// if the pair has been nominated before, just use it
	//nominated=true表示之前收到controlling端携带useCandidate的ping包
	//此时该pair自己ping成功了，就可以直接提名了
	if p.nominated && s.agent.selectedPair == nil {
		s.log.Infof("controlled agent set selected pair: %v", p)
		s.agent.setSelectedPair(FirstConnectedReason, p)
	}
	s.log.Tracef("Found valid candidate pair: %s", p)
}

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
			//如果该pair之前已经成功，现在又收到携带useCandidate属性的包，设置为selectedPair
			if s.agent.selectedPair == nil {
				s.agent.setSelectedPair(FirstConnectedReason, p)
			}
			s.agent.sendBindingSuccess(m, local, remote, 0)
		} else {
			// If the received Binding request triggered a new check to be
			// enqueued in the triggered-check queue (Section 7.3.1.4), once the
			// check is sent and if it generates a successful response, and
			// generates a valid pair, the agent sets the nominated flag of the
			// pair to true.  If the request fails (Section 7.2.5.2), the agent
			// MUST remove the candidate pair from the valid list, set the
			// candidate pair state to Failed, and set the checklist state to
			// Failed.

			// send success response and start ping, if get successful pong
			// then use it.
			s.agent.sendBindingSuccess(m, local, remote, 0)
			s.PingCandidate(p)
			s.log.Tracef("useCandidate set the nominated: %s", p)
			//如果之前没成功，但是收到携带useCandidate属性的包，则先设置nominated = true
			p.nominated = true
		}
	} else {
		//如果支持重提名
		if s.agent.supportRenomination {
			//如果peer端的ping包携带AttrNomination属性，取出nomination值
			if bin, err := m.Get(AttrNomination); err == nil {
				nomination := binary.BigEndian.Uint32(bin)
				if nomination > 0 {
					s.log.Debugf("Renomination-recv nominate attribute %d <--> %d for %v", nomination, s.remoteNomination, p)
					s.agent.sendBindingSuccess(m, local, remote, nomination)
				}
				//nomination为peer端当前ping包携带的nomination值，remoteNomination是记录的上一次peer端的nomination值
				//如果满足nomination > remoteNomination，则进行重提名，并更新remoteNomination值为最新nomination值
				if nomination > s.remoteNomination {
					s.log.Debugf("Renomination-trigger renominate for %v", p)
					//重提名
					s.agent.setSelectedPair(RemoteSwitchReason, p)
					s.remoteNomination = nomination
				}
			} else {
				s.agent.sendBindingSuccess(m, local, remote, 0)
			}
		} else {
			s.agent.sendBindingSuccess(m, local, remote, 0)
		}
		s.PingCandidate(p)
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
