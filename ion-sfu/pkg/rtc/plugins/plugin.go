package plugins

import (
	"errors"
	"sync"

	"github.com/pion/rtp"
	"webrtc/webrtc/ion-sfu/pkg/log"
	"webrtc/webrtc/ion-sfu/pkg/rtc/transport"
)

var (
	errInvalidPlugins = errors.New("invalid plugins, make sure at least one plugin is on")
)

// Plugin some interfaces
type Plugin interface {
	ID() string
	WriteRTP(*rtp.Packet) error
	ReadRTP() <-chan *rtp.Packet
	Stop()
}

const (
	TypeJitterBuffer = "JitterBuffer"
	TypeRTPForwarder = "RTPForwarder"

	maxSize = 100
)

type Config struct {
	On           bool
	JitterBuffer JitterBufferConfig
	RTPForwarder RTPForwarderConfig
}

type PluginChain struct {
	mid        string
	plugins    []Plugin
	pluginLock sync.RWMutex
	stop       bool
	config     Config
}

func NewPluginChain(mid string) *PluginChain {
	return &PluginChain{
		mid: mid,
	}
}

func (p *PluginChain) ReadRTP() *rtp.Packet {
	if p.stop {
		return nil
	}

	// get rtp from the last plugin
	var last Plugin
	p.pluginLock.RLock()
	if len(p.plugins) > 0 {
		last = p.plugins[len(p.plugins)-1]
	}
	p.pluginLock.RUnlock()

	return <-last.ReadRTP()
}

func CheckPlugins(config Config) error {
	log.Logger.Infof("PluginChain.CheckPlugins config=%+v", config)

	//check one plugin is on
	oneOn := false
	if config.JitterBuffer.On {
		oneOn = true
	}

	//check second plugin
	if config.RTPForwarder.On {
		oneOn = true
	}

	if !oneOn {
		return errInvalidPlugins
	}

	return nil
}

func (p *PluginChain) Init(config Config) error {
	p.config = config

	log.Logger.Infof("PluginChain.Init config=%+v", config)
	// first, add JitterBuffer plugin
	if config.JitterBuffer.On {
		log.Logger.Infof("PluginChain.Init config.JitterBuffer.On=true config=%v", config.JitterBuffer)
		config.JitterBuffer.ID = TypeJitterBuffer
		p.AddPlugin(TypeJitterBuffer, NewJitterBuffer(config.JitterBuffer))
	}

	// second, add others
	if config.RTPForwarder.On {
		log.Logger.Infof("PluginChain.Init config.RTPForwarder.On=true config=%v", config.RTPForwarder)
		config.RTPForwarder.ID = TypeRTPForwarder
		config.RTPForwarder.MID = p.mid
		p.AddPlugin(TypeRTPForwarder, NewRTPForwarder(config.RTPForwarder))
	}

	// forward packets along plugin chain
	//类似pipeline的运行方式
	//此处将链式plugins串到一起,原始数据从第一个plugin输入，前一个plugin的输出作为后一个plugin的输入，而最终从最后一个plugin的输出中读取
	for i, plugin := range p.plugins {
		if i == 0 {
			continue
		}
		go func(i int, plugin Plugin) {
			if p.stop {
				return
			}

			for pkt := range p.plugins[i-1].ReadRTP() {
				err := plugin.WriteRTP(pkt)

				if err != nil {
					log.Logger.Errorf("Plugin Forward Packet error => %+v", err)
				}
			}
		}(i, plugin)
	}

	if p.GetPluginsTotal() <= 0 {
		return errInvalidPlugins
	}
	return nil
}

func (p *PluginChain) On() bool {
	return p.config.On
}

func (p *PluginChain) AttachPub(pub transport.Transport) {
	jitterBuffer := p.GetPlugin(TypeJitterBuffer)
	if jitterBuffer != nil {
		log.Logger.Infof("PluginChain.AttachPub pub=%+v", pub)
		jitterBuffer.(*JitterBuffer).AttachPub(pub)
	}
}

// AddPlugin add a plugin
func (p *PluginChain) AddPlugin(id string, i Plugin) {
	p.pluginLock.Lock()
	defer p.pluginLock.Unlock()
	p.plugins = append(p.plugins, i)
}

// GetPlugin get plugin by id
func (p *PluginChain) GetPlugin(id string) Plugin {
	p.pluginLock.RLock()
	defer p.pluginLock.RUnlock()
	for i := 0; i < len(p.plugins); i++ {
		if p.plugins[i].ID() == id {
			return p.plugins[i]
		}
	}
	return nil
}

// GetPluginsTotal get plugin total count
func (p *PluginChain) GetPluginsTotal() int {
	p.pluginLock.RLock()
	defer p.pluginLock.RUnlock()
	return len(p.plugins)
}

// DelPlugin del plugin
func (p *PluginChain) DelPlugin(id string) {
	p.pluginLock.Lock()
	defer p.pluginLock.Unlock()
	for i := 0; i < len(p.plugins); i++ {
		if p.plugins[i].ID() == id {
			p.plugins[i].Stop()
			p.plugins = append(p.plugins[:i], p.plugins[i+1:]...)
		}
	}
}

// DelPluginChain del all plugins
func (p *PluginChain) DelPluginChain() {
	p.pluginLock.Lock()
	defer p.pluginLock.Unlock()
	for _, plugin := range p.plugins {
		plugin.Stop()
	}
	p.plugins = nil
}

func (p *PluginChain) Close() {
	if p.stop {
		return
	}
	p.stop = true
	p.DelPluginChain()
}
