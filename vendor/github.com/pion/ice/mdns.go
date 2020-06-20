package ice

import (
	"net"

	"github.com/pion/logging"
	"github.com/pion/mdns"
	"golang.org/x/net/ipv4"
)

// MulticastDNSMode represents the different Multicast modes ICE can run in
type MulticastDNSMode byte

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
const (
	// MulticastDNSModeDisabled means remote mDNS candidates will be discarded, and local host candidates will use IPs
	MulticastDNSModeDisabled MulticastDNSMode = iota + 1

	// MulticastDNSModeQueryOnly means remote mDNS candidates will be accepted, and local host candidates will use IPs
	MulticastDNSModeQueryOnly

	// MulticastDNSModeQueryAndGather means remote mDNS candidates will be accepted, and local host candidates will use mDNS
	MulticastDNSModeQueryAndGather
)

func generateMulticastDNSName() (string, error) {
	return generateRandString("", ".local")
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
