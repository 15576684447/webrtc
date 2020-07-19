package plugins

import (
	"fmt"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"webrtc/webrtc/ion-sfu/pkg/log"
)

const (
	maxSN      = 65536
	maxPktSize = 1000

	// kProcessIntervalMs=20 ms
	//https://chromium.googlesource.com/external/webrtc/+/ad34dbe934/webrtc/modules/video_coding/nack_module.cc#28

	// vp8 vp9 h264 clock rate 90000Hz
	videoClock = 90000

	//1+16(FSN+BLP) https://tools.ietf.org/html/rfc2032#page-9
	maxNackLostSize = 17

	//default buffer time by ms
	defaultBufferTime = 1000

	tccExtMapID = 3
	//64ms = 64000us = 250 << 8
	//https://webrtc.googlesource.com/src/webrtc/+/f54860e9ef0b68e182a01edc994626d21961bc4b/modules/rtp_rtcp/source/rtcp_packet/transport_feedback.cc#41
	baseScaleFactor = 64000
	//https://webrtc.googlesource.com/src/webrtc/+/f54860e9ef0b68e182a01edc994626d21961bc4b/modules/rtp_rtcp/source/rtcp_packet/transport_feedback.cc#43
	timeWrapPeriodUs = (int64(1) << 24) * baseScaleFactor

	//experiment cycle
	tccCycle = 10 * time.Millisecond
)

func tsDelta(x, y uint32) uint32 {
	if x > y {
		return x - y
	}
	return y - x
}

type rtpExtInfo struct {
	//transport sequence num
	TSN       uint16
	Timestamp int64
}

// Buffer contains all packets
/*
TODO: buffer几个重点功能
	1、pktBuffer循环存放pkt，范围为0~65535,SequenceNumber为int16类型,也为65535
	2、清理过时buffer: 如果上次清理位置的buffer时间戳与当前时间戳差值大于1s，则需要清理过时位置的buffer，并更改lastClearTS/lastClearSN
	3、每16个pkt，发送一组Nack，刚好使用一个uint16变量存储，丢包位置对应bit置1
	4、丢包统计并调整带宽，反馈给发送端(REMB 定时统计)
	5、计算transport-cc-feedback，并反馈给发送端，10ms统计一次
*/
type Buffer struct {
	pktBuffer   [maxSN]*rtp.Packet
	lastNackSN  uint16 //上次Nack发送位置，16个连续pkt为一组，使用一个uint16的16bit表示，如果对应位置丢包，置1
	lastClearTS uint32 //上次清理buffer的时间，如果当前时间与lastClearTS相比大于1s，则从上次清理的位置开始遍历，清理过期pkt
	lastClearSN uint16 //上次清理buffer的位置

	// Last seqnum that has been added to buffer
	lastPushSN uint16 //上次push的位置，即刚push存放的位置

	ssrc        uint32
	payloadType uint8

	//calc lost rate
	receivedPkt int //收到总pkt数
	lostPkt     int //丢失总pkt数

	//response nack channel
	//接收端发送rtcp给发送端的缓冲区
	rtcpCh chan rtcp.Packet

	//calc bandwidth
	totalByte uint64 //接收总字节数

	//buffer time
	maxBufferTS uint32 //数据缓冲时间，超过这个时间的数据被认为是过期数据，则给予清理，默认为1s

	stop bool

	feedbackPacketCount uint8 // transport-cc-feedback包总数

	rtpExtInfoChan chan rtpExtInfo
	// lastTCCSN      uint16
	// bufferStartTS time.Time
}

type BufferOptions struct {
	TCCOn      bool
	BufferTime int
}

// NewBuffer constructs a new Buffer
func NewBuffer(o BufferOptions) *Buffer {
	b := &Buffer{
		rtcpCh:         make(chan rtcp.Packet, maxPktSize),
		rtpExtInfoChan: make(chan rtpExtInfo, maxPktSize),
	}

	if o.TCCOn {
		b.calcTCCLoop()
	}

	if o.BufferTime <= 0 {
		o.BufferTime = defaultBufferTime
	}
	b.maxBufferTS = uint32(o.BufferTime) * videoClock / 1000
	// b.bufferStartTS = time.Now()
	log.Logger.Infof("NewBuffer BufferOptions=%v", o)
	return b
}

/*
transport-cc-feedback，该消息负责反馈接受端收到的所有媒体包的到达时间。
接收端根据包间的接受延迟和发送间隔可以计算出延迟梯度，从而估计带宽。
*/
func (b *Buffer) calcTCCLoop() {
	go func() {
		//每10ms统计一次transport-cc
		t := time.NewTicker(tccCycle)
		defer t.Stop()
		for {
			if b.stop {
				return
			}
			<-t.C
			b.calcTCC()
		}
	}()
}

//计算transport-cc-feedback
func (b *Buffer) calcTCC() {
	cap := len(b.rtpExtInfoChan)
	if cap == 0 {
		return
	}

	//get all rtp extension infos from channel
	//将一段时间内的所有媒体包信息按照sequence-num排序
	rtpExtInfo := make(map[uint16]int64)
	for i := 0; i < cap; i++ {
		info := <-b.rtpExtInfoChan
		rtpExtInfo[info.TSN] = info.Timestamp
	}

	//find the min and max transport sn
	var minTSN, maxTSN uint16
	for tsn := range rtpExtInfo {

		//init
		if minTSN == 0 {
			minTSN = tsn
		}

		if minTSN > tsn {
			minTSN = tsn
		}

		if maxTSN < tsn {
			maxTSN = tsn
		}
	}
	//transport-cc载体数据(表示媒体包到达状态的结构)编码格式选择RunLengthChunk方式
	//force small deta rtcp.RunLengthChunk
	chunk := &rtcp.RunLengthChunk{
		Type:               rtcp.TypeTCCRunLengthChunk,
		PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta,
		RunLength:          maxTSN - minTSN + 1,
	}

	//gather deltas
	var recvDeltas []*rtcp.RecvDelta
	//基准时间，计算该包中每个媒体包的到达时间都要基于这个基准时间计算
	var refTime uint32
	var lastTS int64
	var baseTimeTicks int64
	for i := minTSN; i <= maxTSN; i++ {
		ts, ok := rtpExtInfo[i]

		//lost packet
		//如果对应位置没有媒体包信息，说明对应序列位置包丢失
		if !ok {
			recvDelta := &rtcp.RecvDelta{
				Type: rtcp.TypeTCCPacketReceivedSmallDelta,
			}
			recvDeltas = append(recvDeltas, recvDelta)
			continue
		}

		// init lastTS
		if lastTS == 0 {
			lastTS = ts
		}

		//received packet
		if baseTimeTicks == 0 {
			baseTimeTicks = (ts % timeWrapPeriodUs) / baseScaleFactor
		}
		//将所有后续包的时间戳减去第一个包的时间戳，得到delta
		var delta int64
		if lastTS == ts {
			delta = ts%timeWrapPeriodUs - baseTimeTicks*baseScaleFactor
		} else {
			delta = (ts - lastTS) % timeWrapPeriodUs
		}

		if refTime == 0 {
			refTime = uint32(baseTimeTicks) & 0x007FFFFF
		}

		recvDelta := &rtcp.RecvDelta{
			Type:  rtcp.TypeTCCPacketReceivedSmallDelta,
			Delta: delta,
		}
		recvDeltas = append(recvDeltas, recvDelta)
	}
	rtcpTCC := &rtcp.TransportLayerCC{
		Header: rtcp.Header{
			Padding: false,
			Count:   rtcp.FormatTCC,
			Type:    rtcp.TypeTransportSpecificFeedback,
			// Length:  5, //need calc
		},
		// SenderSSRC:         b.ssrc,
		MediaSSRC:          b.ssrc,
		BaseSequenceNumber: minTSN,                          //当前媒体包序列开始位置
		PacketStatusCount:  maxTSN - minTSN + 1,             //当前媒体序列包总数
		ReferenceTime:      refTime,                         //基准时间，计算该包中每个媒体包的到达时间都要基于这个基准时间计算
		FbPktCount:         b.feedbackPacketCount,           //第几个transport-cc包
		RecvDeltas:         recvDeltas,                      //该媒体包计算的所有时间差序列
		PacketChunks:       []rtcp.PacketStatusChunk{chunk}, //媒体包信息编码类型(共两种，这里选择RunLengthChunk方式)
	}
	rtcpTCC.Header.Length = rtcpTCC.Len()/4 - 1
	if !b.stop {
		b.rtcpCh <- rtcpTCC
		b.feedbackPacketCount++
	}
}

// Push adds a RTP Packet, out of order, new packet may be arrived later
//TODO:SequenceNumber为int16类型，0~65535，和pktBuffer等长
func (b *Buffer) Push(p *rtp.Packet) {
	b.receivedPkt++
	b.totalByte += uint64(p.MarshalSize())

	// init ssrc payloadType
	if b.ssrc == 0 || b.payloadType == 0 {
		b.ssrc = p.SSRC
		b.payloadType = p.PayloadType
	}

	// init lastClearTS
	if b.lastClearTS == 0 {
		b.lastClearTS = p.Timestamp
	}

	// init lastClearSN
	if b.lastClearSN == 0 {
		b.lastClearSN = p.SequenceNumber
	}

	// init lastNackSN
	if b.lastNackSN == 0 {
		b.lastNackSN = p.SequenceNumber
	}
	//根据SequenceNumber存放pkt
	b.pktBuffer[p.SequenceNumber] = p
	b.lastPushSN = p.SequenceNumber

	//store arrival time
	timestampUs := time.Now().UnixNano() / 1000
	rtpTCC := rtp.TransportCCExtension{}
	err := rtpTCC.Unmarshal(p.GetExtension(tccExtMapID))
	if err == nil {
		// if time.Now().Sub(b.bufferStartTS) > time.Second {

		//only calc the packet which rtpTCC.TransportSequence > b.lastTCCSN
		//https://webrtc.googlesource.com/src/webrtc/+/f54860e9ef0b68e182a01edc994626d21961bc4b/modules/rtp_rtcp/source/rtcp_packet/transport_feedback.cc#353
		// if rtpTCC.TransportSequence > b.lastTCCSN {
		b.rtpExtInfoChan <- rtpExtInfo{
			TSN:       rtpTCC.TransportSequence,
			Timestamp: timestampUs,
		}
		// b.lastTCCSN = rtpTCC.TransportSequence
		// }
	}
	// }

	// clear old packet by timestamp
	//TODO:清理缓存过时的buffer，默认为1s；所以整个buffer是一个边存储新数据，边清理过时数据的过程
	b.clearOldPkt(p.Timestamp, p.SequenceNumber)

	// limit nack range
	//当前push的序列号 - 上次没有NCK的序列号
	//控制单次NCK的序列长度
	if b.lastPushSN-b.lastNackSN >= maxNackLostSize {
		b.lastNackSN = b.lastPushSN - maxNackLostSize
	}
	//TODO:此处取这个阈值的原因：GetNackPair使用一个16bit的uint16字段标志连续最多16个pkt丢包位置，如果对应位置丢包，置1
	if b.lastPushSN-b.lastNackSN >= maxNackLostSize {
		// calc [lastNackSN, lastpush-8] if has keyframe
		nackPair, lostPkt := b.GetNackPair(b.pktBuffer, b.lastNackSN, b.lastPushSN)
		b.lastNackSN = b.lastPushSN
		// log.Infof("b.lastNackSN=%v, b.lastPushSN=%v, lostPkt=%v, nackPair=%v", b.lastNackSN, b.lastPushSN, lostPkt, nackPair)
		//如果有丢包，则发送NACK要求对应位置重传
		if lostPkt > 0 {
			b.lostPkt += lostPkt
			nack := &rtcp.TransportLayerNack{
				//origin ssrc
				// SenderSSRC: b.ssrc,
				MediaSSRC: b.ssrc,
				Nacks: []rtcp.NackPair{
					nackPair,
				},
			}
			b.rtcpCh <- nack
		}
	}
}

// clearOldPkt clear old packet
//清理过时的buffer，如果当前pkt和上次清理的pkt时间戳差值大于maxBufferTS，则定义为过时的buffer，则清理其内容
func (b *Buffer) clearOldPkt(pushPktTS uint32, pushPktSN uint16) {
	clearTS := b.lastClearTS
	clearSN := b.lastClearSN
	// log.Infof("clearOldPkt pushPktTS=%d pushPktSN=%d     clearTS=%d  clearSN=%d ", pushPktTS, pushPktSN, clearTS, clearSN)
	if tsDelta(pushPktTS, clearTS) >= b.maxBufferTS { //默认过时数据为1s
		//pushPktSN will loop from 0 to 65535
		if pushPktSN == 0 {
			//make sure clear the old packet from 655xx to 65535
			pushPktSN = maxSN - 1
		}
		var skipCount int
		//遍历从上次清理的位置到现在插入的位置，计算时间差，如果超过maxBufferTS，则给予清理
		for i := clearSN + 1; i <= pushPktSN; i++ {
			if b.pktBuffer[i] == nil {
				skipCount++
				continue
			}
			if tsDelta(pushPktTS, b.pktBuffer[i].Timestamp) >= b.maxBufferTS {
				b.lastClearTS = b.pktBuffer[i].Timestamp
				b.lastClearSN = i
				b.pktBuffer[i] = nil
			} else {
				break
			}
		}
		if skipCount > 0 {
			log.Logger.Infof("b.pktBuffer nil count : %d", skipCount)
		}
		if pushPktSN == maxSN-1 {
			b.lastClearSN = 0
			b.lastNackSN = 0
		}
	}
}

// FindPacket find packet from buffer
func (b *Buffer) FindPacket(sn uint16) *rtp.Packet {
	return b.pktBuffer[sn]
}

// Stop buffer
func (b *Buffer) Stop() {
	b.stop = true
	close(b.rtcpCh)
	b.clear()
}

func (b *Buffer) clear() {
	for i := range b.pktBuffer {
		b.pktBuffer[i] = nil
	}
}

// GetPayloadType get payloadtype
func (b *Buffer) GetPayloadType() uint8 {
	return b.payloadType
}

// GetStat get status from buffer
func (b *Buffer) GetStat() string {
	out := fmt.Sprintf("buffer:[%d, %d] | lastNackSN:%d | lostRate:%.2f |\n", b.lastClearSN, b.lastPushSN, b.lastNackSN, float64(b.lostPkt)/float64(b.receivedPkt+b.lostPkt))
	return out
}

// GetNackPair calc nackpair
func (b *Buffer) GetNackPair(buffer [65536]*rtp.Packet, begin, end uint16) (rtcp.NackPair, int) {

	var lostPkt int

	//size is <= 17
	if end-begin > maxNackLostSize {
		return rtcp.NackPair{}, lostPkt
	}

	//Bitmask of following lost packets (BLP)
	blp := uint16(0)
	lost := uint16(0)

	//find first lost pkt
	//TODO:这里没有到end，即总数为maxNackLostSize-1 = 16，刚好对应uint16
	for i := begin; i < end; i++ {
		if buffer[i] == nil {
			lost = i
			lostPkt++
			break
		}
	}

	//no packet lost
	if lost == 0 {
		return rtcp.NackPair{}, lostPkt
	}

	//calc blp
	//TODO:用一个16bit的uint16，表示连续16个位置，哪些位置有pkt丢失!!!
	for i := lost; i < end; i++ {
		//calc from next lost packet
		if i > lost && buffer[i] == nil {
			blp = blp | (1 << (i - lost - 1))
			lostPkt++
		}
	}
	log.Logger.Tracef("NackPair begin=%v end=%v buffer=%v\n", begin, end, buffer[begin:end])
	return rtcp.NackPair{PacketID: lost, LostPackets: rtcp.PacketBitmap(blp)}, lostPkt
}

// SetSSRCPT set ssrc payloadtype
func (b *Buffer) SetSSRCPT(ssrc uint32, pt uint8) {
	b.ssrc = ssrc
	b.payloadType = pt
}

// GetSSRC get ssrc
func (b *Buffer) GetSSRC() uint32 {
	return b.ssrc
}

// GetRTCPChan return rtcp channel
func (b *Buffer) GetRTCPChan() chan rtcp.Packet {
	return b.rtcpCh
}

// GetLostRateBandwidth calc lostRate and bandwidth by cycle
func (b *Buffer) GetLostRateBandwidth(cycle uint64) (float64, uint64) {
	//计算丢包率
	lostRate := float64(b.lostPkt) / float64(b.receivedPkt+b.lostPkt)
	//计算平均码率
	byteRate := b.totalByte / cycle
	log.Logger.Debugf("Buffer.CalcLostRateByteRate b.receivedPkt=%d b.lostPkt=%d   lostRate=%v byteRate=%v", b.receivedPkt, b.lostPkt, lostRate, byteRate)
	b.receivedPkt, b.lostPkt, b.totalByte = 0, 0, 0
	return lostRate, byteRate * 8 / 1000
}

// GetPacket get packet by sequence number
func (b *Buffer) GetPacket(sn uint16) *rtp.Packet {
	return b.pktBuffer[sn]
}
