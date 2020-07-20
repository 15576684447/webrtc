package rtc

import (
	"fmt"
	"sync"
	"time"

	"webrtc/webrtc"
	"webrtc/webrtc/ion-sfu/pkg/log"
	"webrtc/webrtc/ion-sfu/pkg/rtc/plugins"
	"webrtc/webrtc/ion-sfu/pkg/rtc/rtpengine"
	"webrtc/webrtc/ion-sfu/pkg/rtc/transport"
)

const (
	statCycle = 3 * time.Second
)

var (
	routers    = make(map[string]*Router)
	routerLock sync.RWMutex

	pluginsConfig plugins.Config
	routerConfig  RouterConfig
	stop          bool
)

// InitIce ice urls
func InitIce(iceServers []webrtc.ICEServer, icePortStart, icePortEnd uint16) error {
	//init ice urls and ICE settings
	return transport.InitWebRTC(iceServers, icePortStart, icePortEnd)
}

func InitRouter(config RouterConfig) {
	routerConfig = config
}

// InitPlugins plugins config
func InitPlugins(config plugins.Config) {
	pluginsConfig = config
	log.Logger.Infof("InitPlugins pluginsConfig=%+v", pluginsConfig)
}

// CheckPlugins plugins config
func CheckPlugins(config plugins.Config) error {
	return plugins.CheckPlugins(config)
}

// InitRTP rtp port
func InitRTP(port int, kcpKey, kcpSalt string) error {
	// show stat about all routers
	go check()

	var connCh chan *transport.RTPTransport
	var err error
	// accept relay rtptransport
	if kcpKey != "" && kcpSalt != "" {
		connCh, err = rtpengine.ServeWithKCP(port, kcpKey, kcpSalt)
	} else {
		connCh, err = rtpengine.Serve(port)
	}
	if err != nil {
		log.Logger.Errorf("rtc.InitRPC err=%v", err)
		return err
	}
	go func() {
		for {
			if stop {
				return
			}
			for rtpTransport := range connCh {
				go func(rtpTransport *transport.RTPTransport) {
					id := <-rtpTransport.IDChan

					if id == "" {
						log.Logger.Errorf("invalid id from incoming rtp transport")
						return
					}

					log.Logger.Infof("accept new rtp id=%s conn=%s", id, rtpTransport.RemoteAddr().String())
					if router := AddRouter(id); router != nil {
						router.AddPub(rtpTransport)
					}
				}(rtpTransport)
			}
		}
	}()
	return nil
}

func GetOrNewRouter(id string) *Router {
	log.Logger.Infof("rtc.GetOrNewRouter id=%s", id)
	router := GetRouter(id)
	if router == nil {
		return AddRouter(id)
	}
	return router
}

// GetRouter get router from map
func GetRouter(id string) *Router {
	log.Logger.Infof("rtc.GetRouter id=%s", id)
	routerLock.RLock()
	defer routerLock.RUnlock()
	return routers[id]
}

// AddRouter add a new router
func AddRouter(id string) *Router {
	log.Logger.Infof("rtc.AddRouter id=%s", id)
	routerLock.Lock()
	defer routerLock.Unlock()
	router := NewRouter(id)
	router.OnClose(func() {
		delRouter(id)
	})
	//TODO:构建流水型链式plugins，确定先后顺序 -> 此处将jitterBuffer设置为第一个plugin，数据将从jitterBuffer进入
	if err := router.InitPlugins(pluginsConfig); err != nil {
		log.Logger.Errorf("rtc.AddRouter InitPlugins err=%v", err)
		return nil
	}
	routers[id] = router
	return routers[id]
}

// delRouter delete pub
func delRouter(id string) {
	log.Logger.Infof("delRouter id=%s", id)
	routerLock.Lock()
	defer routerLock.Unlock()
	delete(routers, id)
}

// check show all Routers' stat
func check() {
	t := time.NewTicker(statCycle)
	for range t.C {
		info := "\n----------------rtc-----------------\n"
		print := false
		routerLock.Lock()
		if len(routers) > 0 {
			print = true
		}

		for id, router := range routers {
			info += "pub: " + string(id) + "\n"
			subs := router.GetSubs()
			if len(subs) < 6 {
				for id := range subs {
					info += fmt.Sprintf("sub: %s\n", id)
				}
				info += "\n"
			} else {
				info += fmt.Sprintf("subs: %d\n\n", len(subs))
			}
		}
		routerLock.Unlock()
		if print {
			log.Logger.Infof(info)
		}
	}
}
