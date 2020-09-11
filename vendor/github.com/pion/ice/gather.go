package ice

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/dtls/v2"
	"github.com/pion/logging"
	"github.com/pion/turn/v2"
)

const (
	stunGatherTimeout = time.Second * 5
)

type closeable interface {
	Close() error
}

// Close a net.Conn and log if we have a failure
func closeConnAndLog(c closeable, log logging.LeveledLogger, msg string) {
	if c == nil {
		log.Warnf("Conn is not allocated")
		return
	}

	log.Warnf(msg)
	if err := c.Close(); err != nil {
		log.Warnf("Failed to close conn: %v", err)
	}
}

// fakePacketConn wraps a net.Conn and emulates net.PacketConn
type fakePacketConn struct {
	nextConn net.Conn
}

func (f *fakePacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, err = f.nextConn.Read(p)
	addr = f.nextConn.RemoteAddr()
	return
}
func (f *fakePacketConn) Close() error                       { return f.nextConn.Close() }
func (f *fakePacketConn) LocalAddr() net.Addr                { return f.nextConn.LocalAddr() }
func (f *fakePacketConn) SetDeadline(t time.Time) error      { return f.nextConn.SetDeadline(t) }
func (f *fakePacketConn) SetReadDeadline(t time.Time) error  { return f.nextConn.SetReadDeadline(t) }
func (f *fakePacketConn) SetWriteDeadline(t time.Time) error { return f.nextConn.SetWriteDeadline(t) }
func (f *fakePacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	return f.nextConn.Write(p)
}

// GatherCandidates initiates the trickle based gathering process.
func (a *Agent) GatherCandidates() error {
	gatherErrChan := make(chan error, 1)

	runErr := a.run(func(agent *Agent) {
		if a.gatheringState != GatheringStateNew {
			gatherErrChan <- ErrMultipleGatherAttempted
			return
		} else if a.onCandidateHdlr.Load() == nil {
			gatherErrChan <- ErrNoOnCandidateHandler
			return
		}

		a.gatherCandidates()
		gatherErrChan <- nil
	})
	if runErr != nil {
		return runErr
	}
	return <-gatherErrChan
}

func (a *Agent) gatherCandidates() <-chan struct{} {
	gatherStateUpdated := make(chan bool)

	a.chanCandidate = make(chan Candidate, 1)
	var closeChanCandidateOnce sync.Once
	go func() {
		for c := range a.chanCandidate {
			if onCandidateHdlr, ok := a.onCandidateHdlr.Load().(func(Candidate)); ok {
				onCandidateHdlr(c)
			}
		}
		if onCandidateHdlr, ok := a.onCandidateHdlr.Load().(func(Candidate)); ok {
			onCandidateHdlr(nil)
		}
	}()

	done := make(chan struct{})

	go func() {
		defer func() {
			closeChanCandidateOnce.Do(func() {
				close(a.chanCandidate)
			})
			close(done)
		}()

		if err := a.run(func(agent *Agent) {
			a.gatheringState = GatheringStateGathering
			a.log.Debugf("gatherCandidates: set gatheringState = GatheringStateGathering\n")
			close(gatherStateUpdated)
		}); err != nil {
			a.log.Warnf("failed to set gatheringState to GatheringStateGathering for gatherCandidates: %v", err)
			return
		}
		<-gatherStateUpdated
		//3种gatherCandidatesxxx是重点!!! 底层统一调用了agent.addCandidate函数
		for _, t := range a.candidateTypes {
			switch t {
			case CandidateTypeHost:
				// MulticastDNSMode enum
				//参考文档 https://tools.ietf.org/html/draft-ietf-rtcweb-mdns-ice-candidates-04
				// WebRTC收集ICE candidate作为创建peerConnection流程的一部分
				// 为最大化概率建立p2p连接，客户端私有IP地址也作为candidate。但是，这样涉及到私有地址的隐私问题。
				// 本文介绍了一种与其他客户端共享本地私有IP地址，同时保留客户端隐私的方法。
				// 这是通过动态生成多播DNS（mDNS）名称来隐藏真实私有IP地址来实现的。
				/*
				   发送端搜集candidate实现过程
				   1.检查此IP地址是否可以安全公开。如果可以，则无需使用mDNS方法。否则进行下一步。
				   2.检查ICE agent是否已经生成并注册，并按照步骤3的方法存储此IP地址的mDNS主机名。如果之前有存储，直接跳到步骤6。
				   3.生成唯一的mDNS主机名。唯一的名称必须包含[ RFC4122 ]中定义的版本4的UUID，并添加后缀“.local”。
				   4.按照[ RFC6762 ]中的定义注册candidate的mDNS主机名。ICE agent应发送主机名的mDNS通道，
				   	但是由于主机名是唯一的，因此ICE agent应该跳过对主机名的探测。
				   5.将mDNS主机名及其对应的IP地址存储在ICE agent中以备将来复用。
				   6.使用mDNS替换ICE candidate中的私有IP地址，并提供给Web应用程序。

				   接收端收到remote candidate实现过程
				   1.如果remote ICE candidate的连接地址字段值不以“ .local”结尾或如果值包含多个“.”，则按照[ RFC8445 ]中的定义的方法处理dandidate。
				   2.否则，使用mDNS解析candidate。ICE agent应该使用单播响应mDNS查询；这样可以最大程度地减少多播流量。
				   3.如果解析出IP地址，则替换该mDNS域名对应的主机名，然后继续按照[ RFC8445 ]中定义的方法处理candidate。
				   4.否则，忽略该candidate。
				*/
				a.log.Debugf("gatherCandidates -> gatherCandidatesLocal\n")
				a.gatherCandidatesLocal(a.networkTypes)
			case CandidateTypeServerReflexive:
				//通过指定STUN服务器地址来获取外部IP
				//ICE利用STUN（RFC5389） Binding Request和Response，来获取公网映射地址和进行连通性检查。
				/*
				在Binding request/response事务中，Binding请求从STUN客户端发送到STUN服务器。
				当Binding请求到达STUN服务器时，它可能已经通过STUN客户端和STUN服务器之间的一个或多个nat。
				当Binding请求消息通过NAT时，NAT将修改包的源传输地址（即源IP地址和源端口）。
				因此，服务器收到请求的src addr将是最靠近服务器的NAT公网IP地址和端口，
				该行为被称为传输地址反射(reflexive transport address)。
				STUN服务器将src addr复制到STUN Binding response中的XOR-MAPPED-address属性中，
				并将Binding respons返回给STUN客户端。当包通过NAT返回时，NAT将修改IP报头中的dest addr，
				但STUN body中的XOR-MAPPED-address属性将保持不变。如此客户端就可以获取到自身网络的外网NAT映射结果了。
				*/
				a.log.Debugf("gatherCandidates -> gatherCandidatesSrflx\n")
				a.gatherCandidatesSrflx(a.urls, a.networkTypes)
				//如果指定IPMapper
				if a.extIPMapper != nil && a.extIPMapper.candidateType == CandidateTypeServerReflexive {
					a.log.Debugf("gatherCandidates -> gatherCandidatesSrflxMapped\n")
					a.gatherCandidatesSrflxMapped(a.networkTypes)
				}
			case CandidateTypeRelay:
				/*
				位于NAT后面的主机可能希望与其他位于NAT后面的主机完成数据包交换，此时NAT打洞往往会失败。
				在这种情况下，主机必须使用通信中继服务节点(relay)，这种中继通常位于公网，为两台位于NAT之后的主机中继数据包。
				TURN（Traversal Using Relays around NAT）协议允许主机控制中继服务器的行为，使中继服务器与其对端完成数据包的交换。
				客户端通过获取服务器上的IP地址和端口（称为中继传输地址）来完成此操作。
				TURN与其他一些中继控制协议的不同之处在于，它允许客户端使用一个中继地址与多个对端通信。
				虽然TURN允许使用UDP、TCP或TLS（Transport Layer Security）中的任何一个在客户端和服务器之间传送TURN消息，
				但是TURN在服务器和对端之间总是采用UDP进行数据传输。
				如果客户端和服务器之间使用TCP或TLS，则服务器会将其转换为UDP传输，将数据中继到对端。
				*/
				//使用Relay模式时，发送给对端的candidate地址应该为relay的ip:port，而不是本端的ip:port
				//实际传输时，本端将数据发送到relay服务器，并告知relay服务器将数据中继到对端
				/*
				具体操作为客户端向relay服务器请求分配Allocation，如果分配成功，relay服务器会返回一个成功分配的Allocation
				为保持Allocation不失效，客户端需要定时发送Refresh保活
				客户端到relay服务器有两种数据发送机制：第一种机制使用Send and Data方法，第二种方法使用channels
				Send and Data机制：客户端发送给relay服务端的数据中包含（a）一个XOR-PEER-ADDRESS属性，该属性指定对端的传输地址(NAT映射地址)，以及（b）一个包含数据的data属性
				channels机制：使用ChannelData的备用数据包格式。该格式有一个4字节的头，其中包含一个称为channel number的数字。每个通道号都绑定到特定的对端，因此用作对端主机传输地址的简写
				*/
				a.log.Debugf("gatherCandidates -> gatherCandidatesRelay\n")
				if err := a.gatherCandidatesRelay(a.urls); err != nil {
					a.log.Errorf("Failed to gather relay candidates: %v\n", err)
				}
			}
		}
		if err := a.run(func(agent *Agent) {
			closeChanCandidateOnce.Do(func() {
				close(agent.chanCandidate)
			})
			a.gatheringState = GatheringStateComplete
			a.log.Debugf("gatherCandidates: set gatheringState = GatheringStateGathering\n")
		}); err != nil {
			a.log.Warnf("Failed to stop OnCandidate handler routine and update gatheringState: %v\n", err)
			return
		}
	}()

	return done
}

func (a *Agent) gatherCandidatesLocal(networkTypes []NetworkType) {
	localIPs, err := localInterfaces(a.net, a.interfaceFilter, networkTypes)
	if err != nil {
		a.log.Warnf("failed to iterate local interfaces, host candidates will not be gathered %s", err)
		return
	}
	a.log.Debugf("gatherCandidatesLocal: localIps=%+v\n", localIPs)
	for _, ip := range localIPs {
		a.log.Debugf("gatherCandidatesLocal: current IP = %s\n", ip.String())
		mappedIP := ip
		//如果caididate搜集端和接收端不同时支持mDNS域名模式，解析ip地址对应的外网IP
		if a.mDNSMode != MulticastDNSModeQueryAndGather && a.extIPMapper != nil && a.extIPMapper.candidateType == CandidateTypeHost {
			if _mappedIP, err := a.extIPMapper.findExternalIP(ip.String()); err == nil {
				mappedIP = _mappedIP
				a.log.Debugf("gatherCandidatesLocal: 1:1 NAT mapping is enabled, external IP is %s\n", mappedIP.String())
			} else {
				a.log.Warnf("1:1 NAT mapping is enabled but no external IP is found for %s\n", ip.String())
			}
		}

		address := mappedIP.String()
		//如果caididate搜集端和接收端都支持mDNS域名模式，直接使用域名
		if a.mDNSMode == MulticastDNSModeQueryAndGather {
			a.log.Debugf("gatherCandidatesLocal: MulticastDNSModeQueryAndGather\n")
			address = a.mDNSName
		}

		for _, network := range supportedNetworks {
			conn, err := listenUDPInPortRange(a.net, a.log, int(a.portmax), int(a.portmin), network, &net.UDPAddr{IP: ip, Port: 0})
			if err != nil {
				a.log.Warnf("could not listen %s %s\n", network, ip)
				continue
			}
			port := conn.LocalAddr().(*net.UDPAddr).Port
			hostConfig := CandidateHostConfig{
				Network:   network,
				Address:   address,
				Port:      port,
				Component: ComponentRTP,
			}
			a.log.Debugf("gatherCandidatesLocal: listenUDPInPortRange ok, CandidateHost: %+v\n", hostConfig)
			c, err := NewCandidateHost(&hostConfig)
			if err != nil {
				closeConnAndLog(conn, a.log, fmt.Sprintf("Failed to create host candidate: %s %s %d: %v\n", network, mappedIP, port, err))
				continue
			}
			//如果是mDNS模式，需要设置真实IP
			if a.mDNSMode == MulticastDNSModeQueryAndGather {
				a.log.Debugf("gatherCandidatesLocal: mDNSMode, set ip: %+v\n", ip)
				if err = c.setIP(ip); err != nil {
					closeConnAndLog(conn, a.log, fmt.Sprintf("Failed to create host candidate: %s %s %d: %v\n", network, mappedIP, port, err))
					continue
				}
			}
			a.log.Debugf("gatherCandidatesLocal: addCandidate %+v to candidate list\n", c)
			//此处是重点，每次增加一个Candidate，就为其新建一个专属recvLoop，专门接收并处理该连接上的消息!!!
			if err := a.addCandidate(c, conn); err != nil {
				if closeErr := c.close(); closeErr != nil {
					a.log.Warnf("Failed to close candidate: %v", closeErr)
				}
				a.log.Warnf("Failed to append to localCandidates and run onCandidateHdlr: %v\n", err)
			}
		}
	}
}

func (a *Agent) gatherCandidatesSrflxMapped(networkTypes []NetworkType) {
	for _, networkType := range networkTypes {
		network := networkType.String()

		conn, err := listenUDPInPortRange(a.net, a.log, int(a.portmax), int(a.portmin), network, &net.UDPAddr{IP: nil, Port: 0})
		if err != nil {
			a.log.Warnf("Failed to listen %s: %v\n", network, err)
			continue
		}

		laddr := conn.LocalAddr().(*net.UDPAddr)
		mappedIP, err := a.extIPMapper.findExternalIP(laddr.IP.String())
		if err != nil {
			closeConnAndLog(conn, a.log, fmt.Sprintf("1:1 NAT mapping is enabled but no external IP is found for %s\n", laddr.IP.String()))
			continue
		}
		a.log.Debugf("gatherCandidatesSrflxMapped: findExternalIP %s -> %s\n", laddr.IP.String(), mappedIP.String())
		srflxConfig := CandidateServerReflexiveConfig{
			Network:   network,
			Address:   mappedIP.String(),
			Port:      laddr.Port,
			Component: ComponentRTP,
			RelAddr:   laddr.IP.String(),
			RelPort:   laddr.Port,
		}
		a.log.Debugf("gatherCandidatesSrflxMapped: CandidateServerReflexive %+v\n", srflxConfig)
		c, err := NewCandidateServerReflexive(&srflxConfig)
		if err != nil {
			closeConnAndLog(conn, a.log, fmt.Sprintf("Failed to create server reflexive candidate: %s %s %d: %v\n",
				network,
				mappedIP.String(),
				laddr.Port,
				err))
			continue
		}
		a.log.Debugf("gatherCandidatesSrflxMapped: addCandidate %+v\n", c)
		if err := a.addCandidate(c, conn); err != nil {
			if closeErr := c.close(); closeErr != nil {
				a.log.Warnf("Failed to close candidate: %v", closeErr)
			}
			a.log.Warnf("Failed to append to localCandidates and run onCandidateHdlr: %v\n", err)
		}
	}
}

func (a *Agent) gatherCandidatesSrflx(urls []*URL, networkTypes []NetworkType) {
	var wg sync.WaitGroup
	for _, networkType := range networkTypes {
		for i := range urls {
			if urls[i].Scheme != SchemeTypeSTUN {
				continue
			}

			wg.Add(1)
			go func(url URL, network string) {
				defer wg.Done()
				hostPort := fmt.Sprintf("%s:%d", url.Host, url.Port)
				a.log.Debugf("gatherCandidatesSrflx: hostPort=%s\n", hostPort)
				serverAddr, err := a.net.ResolveUDPAddr(network, hostPort)
				if err != nil {
					a.log.Warnf("failed to resolve stun host: %s: %v", hostPort, err)
					return
				}
				a.log.Debugf("gatherCandidatesSrflx: ResolveUDPAddr=%+v\n", serverAddr)
				conn, err := listenUDPInPortRange(a.net, a.log, int(a.portmax), int(a.portmin), network, &net.UDPAddr{IP: nil, Port: 0})
				if err != nil {
					closeConnAndLog(conn, a.log, fmt.Sprintf("Failed to listen for %s: %v\n", serverAddr.String(), err))
					return
				}
				a.log.Debugf("ResolveUDPAddr: listenUDPInPortRange [%+v] ~ [%+v]\n", a.portmin, a.portmax)
				//请求STUN服务器，并解析返回数据包的XOR-MAPPED-address属性，获取映射公网IP
				a.log.Debugf("ResolveUDPAddr: getXORMappedAddr -> send stun request\n")
				xoraddr, err := getXORMappedAddr(conn, serverAddr, stunGatherTimeout)
				if err != nil {
					closeConnAndLog(conn, a.log, fmt.Sprintf("could not get server reflexive address %s %s: %v\n", network, url, err))
					return
				}

				ip := xoraddr.IP
				port := xoraddr.Port

				laddr := conn.LocalAddr().(*net.UDPAddr)
				srflxConfig := CandidateServerReflexiveConfig{
					Network:   network,
					Address:   ip.String(),
					Port:      port,
					Component: ComponentRTP,
					RelAddr:   laddr.IP.String(),
					RelPort:   laddr.Port,
				}
				a.log.Debugf("getXORMappedAddr: CandidateServerReflexive %+v\n", srflxConfig)
				c, err := NewCandidateServerReflexive(&srflxConfig)
				if err != nil {
					closeConnAndLog(conn, a.log, fmt.Sprintf("Failed to create server reflexive candidate: %s %s %d: %v\n", network, ip, port, err))
					return
				}
				a.log.Debugf("getXORMappedAddr: addCandidate %+v\n", c)
				if err := a.addCandidate(c, conn); err != nil {
					if closeErr := c.close(); closeErr != nil {
						a.log.Warnf("Failed to close candidate: %v", closeErr)
					}
					a.log.Warnf("Failed to append to localCandidates and run onCandidateHdlr: %v\n", err)
				}
			}(*urls[i], networkType.String())
		}
	}

	// Block until all STUN URLs have been gathered (or timed out)
	wg.Wait()
}

func (a *Agent) gatherCandidatesRelay(urls []*URL) error {
	var wg sync.WaitGroup

	network := NetworkTypeUDP4.String() // TODO IPv6
	for i := range urls {
		a.log.Debugf("gatherCandidatesRelay: urls[%d]=%+v\n", i, urls[i])
		switch {
		case urls[i].Scheme != SchemeTypeTURN && urls[i].Scheme != SchemeTypeTURNS:
			continue
		case urls[i].Username == "":
			return ErrUsernameEmpty
		case urls[i].Password == "":
			return ErrPasswordEmpty
		}

		wg.Add(1)
		go func(url URL) {
			defer wg.Done()
			TURNServerAddr := fmt.Sprintf("%s:%d", url.Host, url.Port)
			a.log.Debugf("gatherCandidatesRelay: TurnServerAddr %s, proto=%s, schema=%s\n", TURNServerAddr, url.Proto, url.Scheme)
			var (
				locConn net.PacketConn
				err     error
				RelAddr string
				RelPort int
			)
			/*
			虽然TURN允许使用UDP、TCP或TLS（Transport Layer Security）中的任何一个在客户端和服务器之间传送TURN消息，
			但是TURN在服务器和对端之间总是采用UDP进行数据传输。
			 */
			//根据Proto与Scheme创建与STUN服务器对应的连接
			switch {
			//客户端与TURN服务器采用UDP发送消息
			case url.Proto == ProtoTypeUDP && url.Scheme == SchemeTypeTURN:
				if locConn, err = a.net.ListenPacket(network, "0.0.0.0:0"); err != nil {
					a.log.Warnf("Failed to listen %s: %v\n", network, err)
					return
				}

				RelAddr = locConn.LocalAddr().(*net.UDPAddr).IP.String()
				RelPort = locConn.LocalAddr().(*net.UDPAddr).Port
			//客户端与TURN服务器采用TCP发送消息
			case url.Proto == ProtoTypeTCP && url.Scheme == SchemeTypeTURN:
				tcpAddr, connectErr := net.ResolveTCPAddr(NetworkTypeTCP4.String(), TURNServerAddr)
				if connectErr != nil {
					a.log.Warnf("Failed to resolve TCP Addr %s: %v\n", TURNServerAddr, connectErr)
					return
				}

				conn, connectErr := net.DialTCP(NetworkTypeTCP4.String(), nil, tcpAddr)
				if connectErr != nil {
					a.log.Warnf("Failed to Dial TCP Addr %s: %v\n", TURNServerAddr, connectErr)
					return
				}

				RelAddr = conn.LocalAddr().(*net.TCPAddr).IP.String()
				RelPort = conn.LocalAddr().(*net.TCPAddr).Port
				locConn = turn.NewSTUNConn(conn)
			//客户端与TURN服务器采用UDP+DTLS发送消息
			case url.Proto == ProtoTypeUDP && url.Scheme == SchemeTypeTURNS:
				udpAddr, connectErr := net.ResolveUDPAddr(network, TURNServerAddr)
				if connectErr != nil {
					a.log.Warnf("Failed to resolve UDP Addr %s: %v\n", TURNServerAddr, connectErr)
					return
				}

				conn, connectErr := dtls.Dial(network, udpAddr, &dtls.Config{
					InsecureSkipVerify: a.insecureSkipVerify, //nolint:gosec
				})
				if connectErr != nil {
					a.log.Warnf("Failed to Dial DTLS Addr %s: %v\n", TURNServerAddr, connectErr)
					return
				}

				RelAddr = conn.LocalAddr().(*net.UDPAddr).IP.String()
				RelPort = conn.LocalAddr().(*net.UDPAddr).Port
				locConn = &fakePacketConn{conn}
			//客户端与TURN服务器采用TCP+TLS发送消息
			case url.Proto == ProtoTypeTCP && url.Scheme == SchemeTypeTURNS:
				conn, connectErr := tls.Dial(NetworkTypeTCP4.String(), TURNServerAddr, &tls.Config{
					InsecureSkipVerify: a.insecureSkipVerify, //nolint:gosec
				})
				if connectErr != nil {
					a.log.Warnf("Failed to Dial TLS Addr %s: %v\n", TURNServerAddr, connectErr)
					return
				}
				RelAddr = conn.LocalAddr().(*net.TCPAddr).IP.String()
				RelPort = conn.LocalAddr().(*net.TCPAddr).Port
				locConn = turn.NewSTUNConn(conn)
			default:
				a.log.Warnf("Unable to handle URL in gatherCandidatesRelay %v\n", url)
				return
			}
			a.log.Debugf("gatherCandidatesRelay: turn.NewClient\n")
			//创建turn客户端，采用user+passwd认证方式
			client, err := turn.NewClient(&turn.ClientConfig{
				TURNServerAddr: TURNServerAddr,
				Conn:           locConn,
				Username:       url.Username,
				Password:       url.Password,
				LoggerFactory:  a.loggerFactory,
				Net:            a.net,
			})
			if err != nil {
				closeConnAndLog(locConn, a.log, fmt.Sprintf("Failed to build new turn.Client %s %s\n", TURNServerAddr, err))
				return
			}
			a.log.Debugf("gatherCandidatesRelay: client.Listen\n")
			if err = client.Listen(); err != nil {
				client.Close()
				closeConnAndLog(locConn, a.log, fmt.Sprintf("Failed to listen on turn.Client %s %s\n", TURNServerAddr, err))
				return
			}
			//客户端请求STUN服务器创建Allocate，服务器返回可用的Allocate
			//一旦分配了中继服务器传输地址，客户端必须进行保活。为此，客户端定期向服务器发送Refresh请求
			a.log.Debugf("gatherCandidatesRelay: client.Allocate\n")
			relayConn, err := client.Allocate()
			if err != nil {
				client.Close()
				closeConnAndLog(locConn, a.log, fmt.Sprintf("Failed to allocate on turn.Client %s %s\n", TURNServerAddr, err))
				return
			}

			raddr := relayConn.LocalAddr().(*net.UDPAddr)
			relayConfig := CandidateRelayConfig{
				Network:   network,
				Component: ComponentRTP,
				Address:   raddr.IP.String(),
				Port:      raddr.Port,
				RelAddr:   RelAddr,
				RelPort:   RelPort,
				OnClose: func() error {
					client.Close()
					return locConn.Close()
				},
			}
			a.log.Debugf("gatherCandidatesRelay: CandidateRelay %+v\n", relayConfig)
			candidate, err := NewCandidateRelay(&relayConfig)
			if err != nil {
				if relayConErr := relayConn.Close(); relayConErr != nil {
					a.log.Warnf("Failed to close relay %v", relayConErr)
				}

				client.Close()
				closeConnAndLog(locConn, a.log, fmt.Sprintf("Failed to create relay candidate: %s %s: %v\n", network, raddr.String(), err))
				return
			}
			//turn relay模式下，获取的是turn的candidate
			//客户端将消息发送给STUN服务器，STUN服务器将消息中继到对端
			a.log.Debugf("CandidateRelay: addCandidate %+v\n", candidate)
			if err := a.addCandidate(candidate, relayConn); err != nil {
				if closeErr := candidate.close(); closeErr != nil {
					a.log.Warnf("Failed to close candidate: %v", closeErr)
				}
				a.log.Warnf("Failed to append to localCandidates and run onCandidateHdlr: %v\n", err)
			}
		}(*urls[i])
	}

	// Block until all STUN URLs have been gathered (or timed out)
	wg.Wait()
	return nil
}
