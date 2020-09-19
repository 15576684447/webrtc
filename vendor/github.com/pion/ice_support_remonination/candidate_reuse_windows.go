package ice

import (
	"errors"
	"fmt"
	"net"
)

// CandidateHost is a candidate of type host
type CandidateReuse struct {
	candidateBase

	network string
}

// CandidateHostConfig is the config required to create a new CandidateHost
type CandidateReuseConfig struct {
	CandidateID string
	Network     string
	Address     string
	Port        int
	Component   uint16
}

// NewCandidateHost creates a new host candidate
func NewCandidateReuse(config *CandidateHostConfig) (*CandidateReuse, error) {
	return nil, errors.New("windows not support candidate reuse")
}

func (c *CandidateReuse) setIP(ip net.IP) error {
	networkType, err := determineNetworkType(c.network, ip)
	if err != nil {
		return err
	}

	c.candidateBase.networkType = networkType
	c.candidateBase.resolvedAddr = &net.UDPAddr{IP: ip, Port: c.port}
	return nil
}

func (c *CandidateReuse) writeTo(raw []byte, dst Candidate) (int, error) {

	n, err := c.conn.WriteTo(raw, dst.addr())
	if err != nil {
		return n, fmt.Errorf("failed to send packet: %v", err)
	}
	c.seen(true)
	return n, nil
}

func (c *CandidateReuse) start(a *Agent, conn net.PacketConn) {
	// override candidate base start
}

func makeReuseListenSocket(laddr string) (net.PacketConn, error) {
	return nil, errors.New("make reuse error")
}

func (c *CandidateReuse) close() error {
	d := getDispatcher(c.addr().IP.String(), uint16(c.addr().Port))
	d.UnRegisterCand(c)

	return nil
}
