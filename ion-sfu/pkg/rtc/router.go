package rtc

import (
	"math"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"webrtc/webrtc/ion-sfu/pkg/log"
	"webrtc/webrtc/ion-sfu/pkg/rtc/plugins"
	"webrtc/webrtc/ion-sfu/pkg/rtc/transport"
	"webrtc/webrtc/ion-sfu/pkg/util"
)

const (
	maxWriteErr = 100
)

type RouterConfig struct {
	MinBandwidth uint64 `mapstructure:"minbandwidth"`
	MaxBandwidth uint64 `mapstructure:"maxbandwidth"`
	REMBFeedback bool   `mapstructure:"rembfeedback"`
}

//                                      +--->sub
//                                      |
// pub--->pubCh-->pluginChain-->subCh---+--->sub
//                                      |
//                                      +--->sub
// Router is rtp router
type Router struct {
	id             string
	pub            transport.Transport
	subs           map[string]transport.Transport
	subLock        sync.RWMutex
	stop           bool
	pluginChain    *plugins.PluginChain
	subChans       map[string]chan *rtp.Packet //每个接收者的接收管道，pub收到数据后不会直接发送给sub，而是先发送给每个sub的subChans，然后sub从中获取后发送
	rembChan       chan *rtcp.ReceiverEstimatedMaximumBitrate
	onCloseHandler func()
}

// NewRouter return a new Router
func NewRouter(id string) *Router {
	log.Logger.Infof("NewRouter id=%s", id)
	return &Router{
		id:          id,
		subs:        make(map[string]transport.Transport),
		pluginChain: plugins.NewPluginChain(id),
		subChans:    make(map[string]chan *rtp.Packet),
		rembChan:    make(chan *rtcp.ReceiverEstimatedMaximumBitrate),
	}
}

// InitPlugins initializes plugins for the router
func (r *Router) InitPlugins(config plugins.Config) error {
	log.Logger.Infof("Router.InitPlugins config=%+v", config)
	if r.pluginChain != nil {
		return r.pluginChain.Init(config)
	}
	return nil
}

func (r *Router) start() {
	if routerConfig.REMBFeedback {
		//TODO:接收sub端的REMB反馈，汇总后进一步反馈到sub端上游
		go r.rembLoop()
	}
	go func() {
		if r.pluginChain != nil && r.pluginChain.On() {
			log.Logger.Debugf("Router[%s] pluginChain On, pkt will read from plugin\n", r.id)
		} else {
			log.Logger.Debugf("Router[%s] pluginChain Off, pkt will read from webrtc rtp connection\n", r.id)
		}
		defer util.Recover("[Router.start]")
		for {
			if r.stop {
				return
			}

			var pkt *rtp.Packet
			var err error
			// get rtp from pluginChain or pub
			//TODO:如果使用了plugin，则从最后一个plugin中取出，否则直接从pub的conn中读取
			if r.pluginChain != nil && r.pluginChain.On() {
				pkt = r.pluginChain.ReadRTP()
			} else {
				pkt, err = r.pub.ReadRTP()
				if err != nil {
					log.Logger.Errorf("r.pub.ReadRTP err=%v", err)
					continue
				}
			}
			// log.Debugf("pkt := <-r.subCh %v", pkt)
			if pkt == nil {
				continue
			}
			r.subLock.RLock()
			// Push to client send queues
			for i := range r.GetSubs() {
				// Nonblock sending
				select {
				case r.subChans[i] <- pkt:
				default:
					log.Logger.Errorf("Sub consumer is backed up. Dropping packet")
				}
			}
			r.subLock.RUnlock()
		}
	}()
}

// AddPub add a pub transport to the router
func (r *Router) AddPub(t transport.Transport) transport.Transport {
	log.Logger.Infof("AddPub for Router, id=%s\n", r.id)
	r.pub = t
	/*
	TODO:
		因为此处将jitterBuffer作为链式plugins的第一个，
		所以如果开启了plugins，则将第一个plugin(jitterBuffer)绑定到pub，
		将从pub读取的原始数据写入到jitterBuffer，之后就会沿着链式plugins向下传递，直至最后一个
	 */
	r.pluginChain.AttachPub(t)
	r.start()
	t.OnClose(func() {
		r.Close()
	})
	return t
}

// delPub
func (r *Router) delPub() {
	log.Logger.Infof("Router.delPub %s", r.pub.ID())
	if r.pub != nil {
		r.pub.Close()
	}
	if r.pluginChain != nil {
		r.pluginChain.Close()
	}
	r.pub = nil
}

// GetPub get pub
func (r *Router) GetPub() transport.Transport {
	// log.Infof("Router.GetPub %v", r.pub)
	return r.pub
}

//pub收到数据后，将数据写到每个sub的subChan中，sub从subChan接收数据包并发送到对端
func (r *Router) subWriteLoop(subID string, trans transport.Transport) {
	for pkt := range r.subChans[subID] {
		// log.Infof(" WriteRTP %v:%v to %v PT: %v", pkt.SSRC, pkt.SequenceNumber, trans.ID(), pkt.Header.PayloadType)

		if err := trans.WriteRTP(pkt); err != nil {
			// log.Errorf("wt.WriteRTP err=%v", err)
			// del sub when err is increasing
			if trans.WriteErrTotal() > maxWriteErr {
				r.delSub(trans.ID())
			}
		}
		trans.WriteErrReset()
	}
	log.Logger.Infof("Closing sub writer")
}

/*
TODO:
	根据sub端的REMB反馈，进一步反馈到sub的发送端
	每个sub端都会反馈REMB包，rembLoop搜集这些rtcp包，并取出其中最小的带宽值，作为调整pub带宽的依据发送到pub端的上游
	(sub端的REMB包是根据sub端接收的丢包率计算的，用来调整发送端带宽，并进一步反馈到pub端)
*/
func (r *Router) rembLoop() {
	lastRembTime := time.Now()
	maxRembTime := 200 * time.Millisecond
	rembMin := routerConfig.MinBandwidth
	rembMax := routerConfig.MaxBandwidth
	if rembMin == 0 {
		rembMin = 10000 //10 KBit
	}
	if rembMax == 0 {
		rembMax = 100000000 //100 MBit
	}
	var lowest uint64 = math.MaxUint64
	var rembCount, rembTotalRate uint64

	for pkt := range r.rembChan {
		// Update stats
		rembCount++
		rembTotalRate += pkt.Bitrate
		//TODO:统计sub端反馈的最小码率
		if pkt.Bitrate < lowest {
			lowest = pkt.Bitrate
		}

		// Send upstream if time
		//以maxRembTime为周期检测（200ms）
		if time.Since(lastRembTime) > maxRembTime {
			lastRembTime = time.Now()
			avg := uint64(rembTotalRate / rembCount)

			_ = avg
			target := lowest
			//这里并没有用到平均码率，而是用了最小码率，并根据极值进行了适当调整，将sub反馈的最小码率作为参考，反馈到pub的发送端
			if target < rembMin {
				target = rembMin
			} else if target > rembMax {
				target = rembMax
			}

			newPkt := &rtcp.ReceiverEstimatedMaximumBitrate{
				Bitrate:    target,
				SenderSSRC: 1,
				SSRCs:      pkt.SSRCs,
			}

			log.Logger.Infof("Router.rembLoop send REMB: %+v", newPkt)

			if r.GetPub() != nil {
				err := r.GetPub().WriteRTCP(newPkt)
				if err != nil {
					log.Logger.Errorf("Router.rembLoop err => %+v", err)
				}
			}

			// Reset stats
			rembCount = 0
			rembTotalRate = 0
			lowest = math.MaxUint64
		}
	}
}

//sub端的rtcp feedback，包括PLI、FIR、REMB以及Nack
//其中REMB会进一步反馈到pub端的发送端，作为动态调整pub发送端带宽的依据
func (r *Router) subFeedbackLoop(subID string, trans transport.Transport) {
	for pkt := range trans.GetRTCPChan() {
		if r.stop {
			break
		}
		switch pkt := pkt.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			if r.GetPub() != nil {
				// Request a Key Frame
				log.Logger.Infof("Router got pli: %d", pkt.DestinationSSRC())
				err := r.GetPub().WriteRTCP(pkt) //将PLI以及FIR反馈给pub的发送端
				if err != nil {
					log.Logger.Errorf("Router pli err => %+v", err)
				}
			}
		case *rtcp.ReceiverEstimatedMaximumBitrate:
			if routerConfig.REMBFeedback {
				r.rembChan <- pkt
			}
		case *rtcp.TransportLayerNack:
			// log.Infof("Router got nack: %+v", pkt)
			nack := pkt
			for _, nackPair := range nack.Nacks {
				//重传操作: 如果有plugin，比如jitterBuffer，则直接从jitterBuffer的buffer中获取丢失包重传
				//如果无plugin或者从plugin中获取对应数据包失败，则将Nack转发到pub的发送端，直接从源上请求重传
				if !r.resendRTP(subID, nack.MediaSSRC, nackPair.PacketID) {
					n := &rtcp.TransportLayerNack{
						//origin ssrc
						SenderSSRC: nack.SenderSSRC,
						MediaSSRC:  nack.MediaSSRC,
						Nacks:      []rtcp.NackPair{{PacketID: nackPair.PacketID}},
					}
					if r.pub != nil {
						err := r.GetPub().WriteRTCP(n)
						if err != nil {
							log.Logger.Errorf("Router nack WriteRTCP err => %+v", err)
						}
					}
				}
			}

		default:
		}
	}
	log.Logger.Infof("Closing sub feedback")
}

// AddSub add a sub to router
func (r *Router) AddSub(id string, t transport.Transport) transport.Transport {
	//fix panic: assignment to entry in nil map
	if r.stop {
		return nil
	}
	r.subLock.Lock()
	defer r.subLock.Unlock()
	r.subs[id] = t
	//TODO:每个sub分配一个1000大小的chan，pub端将读取的数据先写入subChans，sub端从对应的subChans读取数据即可
	r.subChans[id] = make(chan *rtp.Packet, 1000)
	log.Logger.Infof("Router.AddSub id=%s t=%p", id, t)

	t.OnClose(func() {
		r.delSub(id)
	})

	// Sub loops
	//sub端从subChans获取数据并发送
	go r.subWriteLoop(id, t)
	//接收sub端下游的feedback并转发至pub端上游
	go r.subFeedbackLoop(id, t)
	return t
}

// GetSub get a sub by id
func (r *Router) GetSub(id string) transport.Transport {
	r.subLock.RLock()
	defer r.subLock.RUnlock()
	// log.Infof("Router.GetSub id=%s sub=%v", id, r.subs[id])
	return r.subs[id]
}

// GetSubs get all subs
func (r *Router) GetSubs() map[string]transport.Transport {
	r.subLock.RLock()
	defer r.subLock.RUnlock()
	// log.Infof("Router.GetSubs len=%v", len(r.subs))
	return r.subs
}

// delSub del sub by id
func (r *Router) delSub(id string) {
	log.Logger.Infof("Router.delSub id=%s", id)
	r.subLock.Lock()
	defer r.subLock.Unlock()
	if r.subs[id] != nil {
		r.subs[id].Close()
	}
	if r.subChans[id] != nil {
		close(r.subChans[id])
	}
	delete(r.subs, id)
	delete(r.subChans, id)
}

// delSubs del all sub
func (r *Router) delSubs() {
	log.Logger.Infof("Router.delSubs")
	r.subLock.RLock()
	keys := make([]string, 0, len(r.subs))
	for k := range r.subs {
		keys = append(keys, k)
	}
	r.subLock.RUnlock()

	for _, id := range keys {
		r.delSub(id)
	}
}

// Close release all
func (r *Router) Close() {
	if r.stop {
		return
	}
	log.Logger.Infof("Router.Close")
	r.onCloseHandler()
	r.delPub()
	r.stop = true
	r.delSubs()
}

// OnClose handler called when router is closed.
func (r *Router) OnClose(f func()) {
	r.onCloseHandler = f
}

func (r *Router) resendRTP(sid string, ssrc uint32, sn uint16) bool {
	if r.pub == nil {
		return false
	}
	hd := r.pluginChain.GetPlugin(plugins.TypeJitterBuffer)
	if hd != nil {
		jb := hd.(*plugins.JitterBuffer)
		pkt := jb.GetPacket(ssrc, sn)
		if pkt == nil {
			// log.Infof("Router.resendRTP pkt not found sid=%s ssrc=%d sn=%d pkt=%v", sid, ssrc, sn, pkt)
			return false
		}
		sub := r.GetSub(sid)
		if sub != nil {
			err := sub.WriteRTP(pkt)
			if err != nil {
				log.Logger.Errorf("router.resendRTP err=%v", err)
			}
			// log.Infof("Router.resendRTP sid=%s ssrc=%d sn=%d", sid, ssrc, sn)
			return true
		}
	}
	return false
}
