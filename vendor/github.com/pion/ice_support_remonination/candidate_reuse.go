// +build linux darwin freebsd netbsd openbsd

package ice_support_remonination

import (
	"context"
	"fmt"
	"net"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
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
	Generation  int
}

// NewCandidateHost creates a new host candidate
func NewCandidateReuse(config *CandidateHostConfig) (*CandidateReuse, error) {
	candidateID := config.CandidateID

	if candidateID == "" {
		var err error
		candidateID, err = generateCandidateID()
		if err != nil {
			return nil, err
		}
	}

	c := &CandidateReuse{
		candidateBase: candidateBase{
			id:            candidateID,
			address:       config.Address,
			candidateType: CandidateTypeHost,
			component:     config.Component,
			port:          config.Port,
			generation:    config.Generation,
		},
		network: config.Network,
	}

	if !strings.HasSuffix(config.Address, ".local") {
		ip := net.ParseIP(config.Address)
		if ip == nil {
			return nil, ErrAddressParseFailed
		}

		if err := c.setIP(ip); err != nil {
			return nil, err
		}
	}
	return c, nil
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
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var opErr error
		err := c.Control(func(fd uintptr) {
			opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		})
		if err != nil {
			return err
		}
		return opErr
	}}

	lp, e := lc.ListenPacket(context.Background(), "udp", laddr)
	if e != nil {
		return nil, e
	}
	err := lp.(*net.UDPConn).SetReadBuffer(16 * 1024 * 1024)
	if err != nil {
		fmt.Errorf("single port socket read buffer too small")
	}
	err = lp.(*net.UDPConn).SetWriteBuffer(16 * 1024 * 1024)
	if err != nil {
		fmt.Errorf("single port socket write buffer too small")
	}
	return lp, nil
}

func (c *CandidateReuse) close() error {
	d := getDispatcher(c.addr().IP.String(), uint16(c.addr().Port))
	d.UnRegisterCand(c)

	return nil
}
