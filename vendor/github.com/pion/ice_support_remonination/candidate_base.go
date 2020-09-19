package ice_support_remonination

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun"
)

type candidateBase struct {
	id            string
	networkType   NetworkType
	candidateType CandidateType

	component      uint16
	generation     int
	address        string
	port           int
	relatedAddress *CandidateRelatedAddress

	resolvedAddr *net.UDPAddr

	lastSent     atomic.Value
	lastReceived atomic.Value
	conn         net.PacketConn

	currAgent *Agent
	closeCh   chan struct{}
	closedCh  chan struct{}
}

// ID returns Candidate ID
func (c *candidateBase) ID() string {
	return c.id
}

// Address returns Candidate Address
func (c *candidateBase) Address() string {
	return c.address
}

// Port returns Candidate Port
func (c *candidateBase) Port() int {
	return c.port
}

// Generation retruns Candidate Port
func (c *candidateBase) Generation() int {
	return c.generation
}

// SetGeneration set the candidate generation
func (c *candidateBase) SetGeneration(generation int) {
	c.generation = generation
}

// Type returns candidate type
func (c *candidateBase) Type() CandidateType {
	return c.candidateType
}

// NetworkType returns candidate NetworkType
func (c *candidateBase) NetworkType() NetworkType {
	return c.networkType
}

// Component returns candidate component
func (c *candidateBase) Component() uint16 {
	return c.component
}

// LocalPreference returns the local preference for this candidate
func (c *candidateBase) LocalPreference() uint16 {
	return defaultLocalPreference
}

// RelatedAddress returns *CandidateRelatedAddress
func (c *candidateBase) RelatedAddress() *CandidateRelatedAddress {
	return c.relatedAddress
}

// start runs the candidate using the provided connection
func (c *candidateBase) start(a *Agent, conn net.PacketConn) {
	c.currAgent = a
	c.conn = conn

	go c.recvLoop()
}

func (c *candidateBase) recvLoop() {
	defer CheckPanic()

	log := c.agent().log
	buffer := make([]byte, receiveMTU)
	for {
		n, srcAddr, err := c.conn.ReadFrom(buffer)
		if err != nil {
			return
		}

		handleInboundCandidateMsg(c, buffer[:n], srcAddr, log)
	}
}

func handleInboundCandidateMsg(c Candidate, buffer []byte, srcAddr net.Addr, log logging.LeveledLogger) {
	if stun.IsMessage(buffer) {
		m := &stun.Message{
			Raw: make([]byte, len(buffer)),
		}
		// Explicitly copy raw buffer so Message can own the memory.
		copy(m.Raw, buffer)
		if err := m.Decode(); err != nil {
			log.Warnf("Failed to handle decode ICE from %s to %s: %v", c.addr(), srcAddr, err)
			return
		}
		err := c.agent().run(func(agent *Agent) {
			agent.handleInbound(m, c, srcAddr)
		})
		if err != nil {
			log.Warnf("Failed to handle message: %v", err)
		}

		return
	}

	isValidRemoteCandidate := make(chan bool, 1)
	err := c.agent().run(func(agent *Agent) {
		isValidRemoteCandidate <- agent.noSTUNSeen(c, srcAddr)
	})

	if err != nil {
		log.Warnf("Failed to handle message: %v", err)
	} else if !<-isValidRemoteCandidate {
		log.Warnf("Discarded message from %s, not a valid remote candidate", c.addr())
	}

	// NOTE This will return ice.ErrFull if the buffer ever manages to fill up.
	var n int
	if IsValidPointerInterface(c.agent().dataHandler) {
		n, err = c.agent().dataHandler.Process(buffer)
	}
	if n == 0 {
		if _, err := c.agent().buffer.Write(buffer); err != nil {
			log.Warnf("failed to write packet")
		}
	}
}

// close stops the recvLoop
func (c *candidateBase) close() error {
	if c.conn != nil {
		// Unblock recvLoop
		// Close the conn
		err := c.conn.Close()
		if err != nil {
			return err
		}

		// Wait until the recvLoop is closed
		//<-c.closedCh
	}

	return nil
}

func (c *candidateBase) writeTo(raw []byte, dst Candidate) (int, error) {
	c.seen(true)
	n, err := c.conn.WriteTo(raw, dst.addr())
	if err != nil {
		return n, fmt.Errorf("failed to send packet: %v", err)
	}
	return n, nil
}

// Priority computes the priority for this ICE Candidate
func (c *candidateBase) Priority() uint32 {
	// The local preference MUST be an integer from 0 (lowest preference) to
	// 65535 (highest preference) inclusive.  When there is only a single IP
	// address, this value SHOULD be set to 65535.  If there are multiple
	// candidates for a particular component for a particular data stream
	// that have the same type, the local preference MUST be unique for each
	// one.
	return (1<<24)*uint32(c.Type().Preference()) +
		(1<<8)*uint32(c.LocalPreference()) +
		uint32(256-c.Component())
}

// Equal is used to compare two candidateBases
func (c *candidateBase) Equal(other Candidate) bool {
	return c.NetworkType() == other.NetworkType() &&
		//c.Type() == other.Type() &&
		c.Address() == other.Address() &&
		c.Port() == other.Port()
	//&& c.RelatedAddress().Equal(other.RelatedAddress())
}

// String makes the candidateBase printable
func (c *candidateBase) String() string {
	return fmt.Sprintf("(%s %s:%d%s %d)", c.Type(), c.Address(), c.Port(), c.relatedAddress, c.generation)
}

// LastReceived returns a time.Time indicating the last time
// this candidate was received
func (c *candidateBase) LastReceived() time.Time {
	return c.lastReceived.Load().(time.Time)
}

func (c *candidateBase) setLastReceived(t time.Time) {
	c.lastReceived.Store(t)
}

// LastSent returns a time.Time indicating the last time
// this candidate was sent
func (c *candidateBase) LastSent() time.Time {
	return c.lastSent.Load().(time.Time)
}

func (c *candidateBase) setLastSent(t time.Time) {
	c.lastSent.Store(t)
}

func (c *candidateBase) seen(outbound bool) {
	if outbound {
		c.setLastSent(time.Now())
	} else {
		c.setLastReceived(time.Now())
	}
}

func (c *candidateBase) addr() *net.UDPAddr {
	return c.resolvedAddr
}

func (c *candidateBase) agent() *Agent {
	return c.currAgent
}
