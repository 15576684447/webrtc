package ice

import (
	"fmt"
	"math"
	"time"

	"github.com/pion/stun"
)

const (
	//rtt rollback is the latest time of caculate rtt
	rttRollback = 30 * time.Second
	//max reserve rtt count
	recodeRttCount = 5
)

func newCandidatePair(local, remote Candidate, controlling bool) *candidatePair {
	return &candidatePair{
		iceRoleControlling: controlling,
		remote:             remote,
		local:              local,
		state:              CandidatePairStateWaiting,
		lastPingTime:       time.Now().Add(-startConnectivityCheckPaceTime * time.Millisecond),
		lastPongTime:       time.Now().Add(-startConnectivityCheckPaceTime * time.Millisecond),
	}
}

// CandidateRtts represents a combination of candidatePair's rtt
type CandidateAttrs struct {
	latestRtts []time.Duration
	latestTime []time.Time
	lostCount  int
	pingTimes  []time.Time

	Rtt       float64
	LostRate  int
	NetJitter float32
}

func (rtts *CandidateAttrs) AddLost() {
	rtts.lostCount++
	if rtts.lostCount > 15 {
		rtts.lostCount = 15
	}
}

func (rtts *CandidateAttrs) AddPing() {
	rtts.pingTimes = append(rtts.pingTimes, time.Now())
	if len(rtts.pingTimes) > 15 {
		rtts.pingTimes = rtts.pingTimes[1:]
	}
	total := 0
	for _, t := range rtts.pingTimes {
		if time.Since(t) < 5*time.Second {
			total++
		}
	}
	rtts.LostRate = rtts.lostCount * 100 / total
}
func (rtts *CandidateAttrs) ClearPingTimes() {
	rtts.LostRate = 0
	rtts.lostCount = 0
	rtts.pingTimes = rtts.pingTimes[:0]

}

// AddRtt is add a rtt time duration to list
func (rtts *CandidateAttrs) AddRtt(t time.Duration) {
	rtts.latestRtts = append(rtts.latestRtts, t)
	rtts.latestTime = append(rtts.latestTime, time.Now())
	if len(rtts.latestRtts) > recodeRttCount {
		rtts.latestRtts = rtts.latestRtts[1:]
		rtts.latestTime = rtts.latestTime[1:]
	}
	rtts.Calculate()
}

// GetRtt is get the average rtt of last rttRollback time
func (rtts *CandidateAttrs) Calculate() {
	var duration time.Duration
	var count, jitterCount int64
	lastRtt := time.Duration(0)
	jitter := int64(0)

	for i, d := range rtts.latestTime {
		if time.Since(d) < rttRollback {
			duration += rtts.latestRtts[i]
			if lastRtt > 0 {
				jitter += int64(math.Abs(float64(rtts.latestRtts[i] - lastRtt)))
				jitterCount++
			}
			lastRtt = rtts.latestRtts[i]
			count++
		}
	}
	if jitterCount > 0 {
		rtts.NetJitter = float32(jitter) / float32(jitterCount*int64(time.Millisecond))
	}
	if count > 0 {
		rtts.Rtt = float64(int64(duration) / (count * int64(time.Millisecond)))
	}
}

func (rtts *CandidateAttrs) String() string {
	return fmt.Sprintf("rtt %.2f lostrate %d netjitter %.2f", rtts.Rtt, rtts.LostRate, rtts.NetJitter)
}

// candidatePair represents a combination of a local and remote candidate
type candidatePair struct {
	iceRoleControlling  bool
	remote              Candidate
	local               Candidate
	candidateAttrs      CandidateAttrs
	bindingRequestCount uint16
	state               CandidatePairState
	nominated           bool
	generation          int64
	priority            uint64
	pingTimes           uint64
	lastPingTime        time.Time
	lastPongTime        time.Time
}

func (p *candidatePair) AddRtt(t time.Duration) {
	p.candidateAttrs.AddRtt(t)
}

// Attrs is return attributes of candidatePair
func (p *candidatePair) Attrs() *CandidateAttrs {
	return &p.candidateAttrs
}

func (p *candidatePair) String() string {
	return fmt.Sprintf("prio %d attrs(%s)  state %d (local, prio %d) %s <-> %s (remote, prio %d)",
		p.Priority(), p.Attrs().String(), p.state, p.local.Priority(), p.local, p.remote, p.remote.Priority())
}

func (p *candidatePair) Equal(other *candidatePair) bool {
	if p == nil && other == nil {
		return true
	}
	if p == nil || other == nil {
		return false
	}
	return p.local.Equal(other.local) && p.remote.Equal(other.remote)
}

// RFC 5245 - 5.7.2.  Computing Pair Priority and Ordering Pairs
// Let G be the priority for the candidate provided by the controlling
// agent.  Let D be the priority for the candidate provided by the
// controlled agent.
// pair priority = 2^32*MIN(G,D) + 2*MAX(G,D) + (G>D?1:0)
func (p *candidatePair) Priority() uint64 {
	var g uint32
	var d uint32
	if p.iceRoleControlling {
		g = p.local.Priority()
		d = p.remote.Priority()
	} else {
		g = p.remote.Priority()
		d = p.local.Priority()
	}

	// Just implement these here rather
	// than fooling around with the mathutil package
	min := func(x, y uint32) uint64 {
		if x < y {
			return uint64(x)
		}
		return uint64(y)
	}
	max := func(x, y uint32) uint64 {
		if x > y {
			return uint64(x)
		}
		return uint64(y)
	}
	cmp := func(x, y uint32) uint64 {
		if x > y {
			return uint64(1)
		}
		return uint64(0)
	}

	// 1<<32 overflows uint32; and if both g && d are
	// maxUint32, this result would overflow uint64
	p.priority = (1<<32-1)*min(g, d) + 2*max(g, d) + cmp(g, d)
	return p.priority
}
func (c *candidatePair) Generation(max int) int {
	distance := c.local.Generation() - c.remote.Generation()
	if distance < 0 {
		distance = -distance
	}
	if c.local.Generation() == c.remote.Generation() {
		return c.local.Generation() - distance + 64
	} else {
		return c.local.Generation() - distance - max + 64
	}
}

func (p *candidatePair) Write(b []byte) (int, error) {
	return p.local.writeTo(b, p.remote)
}

func (a *Agent) sendSTUN(msg *stun.Message, local, remote Candidate) {
	_, err := local.writeTo(msg.Raw, remote)
	if err != nil {
		a.log.Tracef("failed to send STUN message: %s", err)
	} else {
		switch msg.Type.Class {
		case stun.ClassRequest:
			a.callback(SendRequest, local.String(), remote.String())
		case stun.ClassSuccessResponse:
			a.callback(SendSuccessResponse, local.String(), remote.String())
		case stun.ClassErrorResponse:
			a.callback(SendErrorResponse, local.String(), remote.String())
		}
	}
}
