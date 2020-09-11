// Package cmd contains an entrypoint for running an ion-sfu instance.
package main

import (
	"webrtc/webrtc"
	conf "webrtc/webrtc/ion-sfu/pkg/conf"
	"webrtc/webrtc/ion-sfu/pkg/log"
	sfu "webrtc/webrtc/ion-sfu/pkg/node"
	"webrtc/webrtc/ion-sfu/pkg/rtc"
	"webrtc/webrtc/ion-sfu/pkg/rtc/plugins"
)

/*
对于一个项目中多个init函数的执行顺序：
1、在main包中的go文件默认总是被执行
2、同包下的不同go文件，按照文件名"从小到大"排序顺序执行
3、其他的包只有被main包import才会执行，按照import的先后顺序执行
4、被递归import的包初始化顺序与import顺序相反，如倒入顺序main->A->B->C，则初始化顺序为C->B->A->main
5、一个包被多个包import，只能被执行一次
6、main包总是被最后一个初始化，其依赖包先与main包初始化
7、避免出现循环import
 */
func init() {
	var icePortStart, icePortEnd uint16

	if len(conf.WebRTC.ICEPortRange) == 2 {
		icePortStart = conf.WebRTC.ICEPortRange[0]
		icePortEnd = conf.WebRTC.ICEPortRange[1]
	}

	log.Init(conf.Log.Level)
	var iceServers []webrtc.ICEServer
	//如果sfu在NAT之后，需要设置iceserver
	for _, iceServer := range conf.WebRTC.ICEServers {
		s := webrtc.ICEServer{
			URLs:       iceServer.URLs,
			Username:   iceServer.Username,
			Credential: iceServer.Credential,
		}
		iceServers = append(iceServers, s)
	}
	if err := rtc.InitIce(iceServers, icePortStart, icePortEnd); err != nil {
		panic(err)
	}
	log.Logger.Debugf("icePortRange: [%d]~[%d], iceServer: %+v\n", icePortStart, icePortEnd, iceServers)
	//监听RTP连接，并初始化对应Router、Pub以及pluginChain(Pipeline)
	if err := rtc.InitRTP(conf.Rtp.Port, conf.Rtp.KcpKey, conf.Rtp.KcpSalt); err != nil {
		panic(err)
	}

	pluginConfig := plugins.Config{
		On: conf.Plugins.On,
		JitterBuffer: plugins.JitterBufferConfig{
			On:            conf.Plugins.JitterBuffer.On,
			TCCOn:         conf.Plugins.JitterBuffer.TCCOn,
			REMBCycle:     conf.Plugins.JitterBuffer.REMBCycle,
			PLICycle:      conf.Plugins.JitterBuffer.PLICycle,
			MaxBandwidth:  conf.Plugins.JitterBuffer.MaxBandwidth,
			MaxBufferTime: conf.Plugins.JitterBuffer.MaxBufferTime,
		},
		RTPForwarder: plugins.RTPForwarderConfig{
			On:      conf.Plugins.RTPForwarder.On,
			Addr:    conf.Plugins.RTPForwarder.Addr,
			KcpKey:  conf.Plugins.RTPForwarder.KcpKey,
			KcpSalt: conf.Plugins.RTPForwarder.KcpSalt,
		},
	}
	//至少得有一个plugin
	if err := rtc.CheckPlugins(pluginConfig); err != nil {
		panic(err)
	}
	//设置plugin与router配置项
	rtc.InitPlugins(pluginConfig)
	rtc.InitRouter(*conf.Router)
}

func main() {
	log.Logger.Infof("--- Starting SFU Node ---")
	sfu.Init(conf.GRPC.Port)
	select {}
}
