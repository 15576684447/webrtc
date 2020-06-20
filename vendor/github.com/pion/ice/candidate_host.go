package ice

import (
	"net"
	"strings"
)

// CandidateHost is a candidate of type host
type CandidateHost struct {
	candidateBase

	network string
}

// CandidateHostConfig is the config required to create a new CandidateHost
type CandidateHostConfig struct {
	CandidateID string
	Network     string
	Address     string
	Port        int
	Component   uint16
}

// NewCandidateHost creates a new host candidate
func NewCandidateHost(config *CandidateHostConfig) (*CandidateHost, error) {
	candidateID := config.CandidateID

	if candidateID == "" {
		var err error
		candidateID, err = generateCandidateID()
		if err != nil {
			return nil, err
		}
	}

	c := &CandidateHost{
		candidateBase: candidateBase{
			id:            candidateID,
			address:       config.Address,
			candidateType: CandidateTypeHost,
			component:     config.Component,
			port:          config.Port,
		},
		network: config.Network,
	}
	//如果Local private IP使用mDNS方式管理，则会携带".local"后缀；否则直接为IP地址
	//如果先按照mDNS方式解析，失败后按照IP解析
	if !strings.HasSuffix(config.Address, ".local") {
		ip := net.ParseIP(config.Address)
		if ip == nil {
			return nil, ErrAddressParseFailed
		}
		//设置真实IP
		if err := c.setIP(ip); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *CandidateHost) setIP(ip net.IP) error {
	networkType, err := determineNetworkType(c.network, ip)
	if err != nil {
		return err
	}

	c.candidateBase.networkType = networkType
	c.candidateBase.resolvedAddr = &net.UDPAddr{IP: ip, Port: c.port}
	return nil
}
