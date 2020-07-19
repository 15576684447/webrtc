package plugins

import (
	"errors"
	"fmt"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"webrtc/webrtc/ion-sfu/pkg/log"
	"webrtc/webrtc/ion-sfu/pkg/rtc/transport"
)

const (
	// bandwidth range(kbps)
	minBandwidth = 200
	maxREMBCycle = 5
	maxPLICycle  = 5
)

// JitterBufferConfig .
type JitterBufferConfig struct {
	ID            string
	On            bool
	TCCOn         bool
	REMBCycle     int
	PLICycle      int
	MaxBandwidth  int
	MaxBufferTime int
}

// JitterBuffer core buffer module
type JitterBuffer struct {
	buffers   map[uint32]*Buffer
	stop      bool
	bandwidth uint64
	lostRate  float64

	config     JitterBufferConfig
	Pub        transport.Transport
	outRTPChan chan *rtp.Packet
}

// NewJitterBuffer return new JitterBuffer
func NewJitterBuffer(config JitterBufferConfig) *JitterBuffer {
	j := &JitterBuffer{
		buffers:    make(map[uint32]*Buffer),
		outRTPChan: make(chan *rtp.Packet, maxSize),
	}
	j.Init(config)
	j.rembLoop()
	j.pliLoop()
	return j
}

// Init jitterbuffer config
func (j *JitterBuffer) Init(config JitterBufferConfig) {
	j.config = config
	log.Logger.Infof("JitterBuffer.Init j.config=%+v", j.config)
	if j.config.REMBCycle > maxREMBCycle {
		j.config.REMBCycle = maxREMBCycle
	}

	if j.config.PLICycle > maxPLICycle {
		j.config.PLICycle = maxPLICycle
	}

	if j.config.MaxBandwidth < minBandwidth {
		j.config.MaxBandwidth = minBandwidth
	}

	log.Logger.Infof("JitterBuffer.Init ok  j.config=%v", j.config)
}

// ID return id
func (j *JitterBuffer) ID() string {
	return j.config.ID
}

// AttachPub Attach pub stream
//AttachPub后，webrtc将读取的数据包写入jitterBuffer，而不是直接转发
func (j *JitterBuffer) AttachPub(t transport.Transport) {
	j.Pub = t
	go func() {
		for {
			if j.stop {
				return
			}
			//从webrtc读取
			pkt, err := j.Pub.ReadRTP()
			if err != nil {
				log.Logger.Errorf("AttachPub j.Pub.ReadRTP pkt=%+v", pkt)
				continue
			}
			//写入jitterBuffer
			err = j.WriteRTP(pkt)
			if err != nil {
				log.Logger.Errorf("AttachPub j.WriteRTP err=%+v", err)
				continue
			}
		}
	}()
}

// AddBuffer add a buffer by ssrc
func (j *JitterBuffer) AddBuffer(ssrc uint32) *Buffer {
	log.Logger.Infof("JitterBuffer.AddBuffer ssrc=%d", ssrc)
	o := BufferOptions{
		TCCOn:      j.config.TCCOn,
		BufferTime: j.config.MaxBufferTime,
	}
	b := NewBuffer(o)
	j.buffers[ssrc] = b
	//发送rtcp协程
	j.rtcpLoop(b)
	return b
}

// GetBuffer get a buffer by ssrc
func (j *JitterBuffer) GetBuffer(ssrc uint32) *Buffer {
	return j.buffers[ssrc]
}

// GetBuffers get all buffers
func (j *JitterBuffer) GetBuffers() map[uint32]*Buffer {
	return j.buffers
}

// WriteRTP push rtp packet which from pub
func (j *JitterBuffer) WriteRTP(pkt *rtp.Packet) error {
	ssrc := pkt.SSRC
	pt := pkt.PayloadType

	// only video, because opus doesn't need nack, use fec: `a=fmtp:111 minptime=10;useinbandfec=1`
	if transport.IsVideo(pt) {
		buffer := j.GetBuffer(ssrc)
		if buffer == nil {
			buffer = j.AddBuffer(ssrc)
			log.Logger.Infof("JitterBuffer.WriteRTP buffer.SetSSRCPT(%d,%d)", ssrc, pt)
			buffer.SetSSRCPT(ssrc, pt)
		}

		if buffer == nil {
			return errors.New("buffer is nil")
		}

		buffer.Push(pkt)
	}
	j.outRTPChan <- pkt
	return nil
}

// ReadRTP return the last packet
func (j *JitterBuffer) ReadRTP() <-chan *rtp.Packet {
	return j.outRTPChan
}

//读取rtcp包并发送，包括Nack、transport-cc-feedback等
func (j *JitterBuffer) rtcpLoop(b *Buffer) {
	go func() {
		for pkt := range b.GetRTCPChan() {
			if j.stop {
				return
			}
			if j.Pub == nil {
				continue
			}
			err := j.Pub.WriteRTCP(pkt)
			if err != nil {
				log.Logger.Errorf("JitterBuffer.rtcpLoop j.Pub.WriteRTCP err=%v", err)
			}
		}
	}()
}

//接收端带宽估计并反馈到发送端
func (j *JitterBuffer) rembLoop() {
	go func() {
		for {
			if j.stop {
				return
			}

			if j.config.REMBCycle <= 0 {
				time.Sleep(time.Second)
				continue
			}
			//每隔一定时间计算一次
			time.Sleep(time.Duration(j.config.REMBCycle) * time.Second)
			for _, buffer := range j.GetBuffers() {
				// only calc video recently
				if !transport.IsVideo(buffer.GetPayloadType()) {
					continue
				}
				/*
					基于丢包的带宽估计(kb)
					1、首先当启动时，此时没有带宽，则以最大带宽进行传输；
					2、当丢包率大于10%时则认为网络有拥塞，此时根据丢包率降低带宽，丢包率越高带宽降的越多；
					3、当丢包率在10%内时则网络状态良好，此时将带宽提升到之前的两倍；
				*/
				j.lostRate, j.bandwidth = buffer.GetLostRateBandwidth(uint64(j.config.REMBCycle))
				var bw uint64
				if j.lostRate == 0 && j.bandwidth == 0 {
					bw = uint64(j.config.MaxBandwidth)
				} else if j.lostRate >= 0 && j.lostRate < 0.1 {
					bw = uint64(j.bandwidth * 2)
				} else {
					bw = uint64(float64(j.bandwidth) * (1 - j.lostRate))
				}
				//带宽调整的上限和下限(200kb ~ 上限)
				if bw < minBandwidth {
					bw = minBandwidth
				}

				if bw > uint64(j.config.MaxBandwidth) {
					bw = uint64(j.config.MaxBandwidth)
				}
				//这是带宽估计的早期实现，评估的带宽结果通过RTCP REMB消息反馈到发送端
				//在新近的WebRTC的实现中，所有的带宽估计都放在了发送端
				remb := &rtcp.ReceiverEstimatedMaximumBitrate{
					SenderSSRC: buffer.GetSSRC(),
					Bitrate:    bw * 1000,
					SSRCs:      []uint32{buffer.GetSSRC()},
				}

				if j.Pub == nil {
					continue
				}
				err := j.Pub.WriteRTCP(remb)
				if err != nil {
					log.Logger.Errorf("JitterBuffer.rembLoop j.Pub.WriteRTCP err=%v", err)
				}
			}
		}
	}()
}

//图片丢失提示，接收端要求发送端周期性的重传一个I帧
func (j *JitterBuffer) pliLoop() {
	go func() {
		for {
			if j.stop {
				return
			}

			if j.config.PLICycle <= 0 {
				time.Sleep(time.Second)
				continue
			}
			//每隔一段时间计算一次
			time.Sleep(time.Duration(j.config.PLICycle) * time.Second)
			for _, buffer := range j.GetBuffers() {
				if transport.IsVideo(buffer.GetPayloadType()) {
					pli := &rtcp.PictureLossIndication{SenderSSRC: buffer.GetSSRC(), MediaSSRC: buffer.GetSSRC()}
					if j.Pub == nil {
						continue
					}
					// log.Infof("pliLoop send pli=%d pt=%v", buffer.GetSSRC(), buffer.GetPayloadType())
					err := j.Pub.WriteRTCP(pli)
					if err != nil {
						log.Logger.Errorf("JitterBuffer.pliLoop j.Pub.WriteRTCP err=%v", err)
					}
				}
			}
		}
	}()
}

// GetPacket get packet from buffer
func (j *JitterBuffer) GetPacket(ssrc uint32, sn uint16) *rtp.Packet {
	buffer := j.buffers[ssrc]
	if buffer == nil {
		return nil
	}
	return buffer.GetPacket(sn)
}

// Stop stop all buffer
func (j *JitterBuffer) Stop() {
	if j.stop {
		return
	}
	j.stop = true
	for _, buffer := range j.buffers {
		buffer.Stop()
	}
	j.buffers = nil
}

// Stat get stat from buffers
func (j *JitterBuffer) Stat() string {
	out := ""
	for ssrc, buffer := range j.buffers {
		out += fmt.Sprintf("ssrc:%d payload:%d | lostRate:%.2f | bandwidth:%dkbps | %s", ssrc, buffer.GetPayloadType(), j.lostRate, j.bandwidth, buffer.GetStat())
	}
	return out
}
