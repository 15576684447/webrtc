package main

import (
	"fmt"
	"github.com/pion/logging"
	"io"
	"time"
	"webrtc/webrtc"
	"webrtc/webrtc/examples/internal/signal"

	"github.com/pion/rtcp"
)

const (
	rtcpPLIInterval = time.Second * 3
)

type customLogger struct {
}

// Print all messages except trace
func (c customLogger) Trace(msg string)                          {}
func (c customLogger) Tracef(format string, args ...interface{}) {}

func (c customLogger) Debug(msg string) { fmt.Printf("customLogger Debug: %s\n", msg) }
func (c customLogger) Debugf(format string, args ...interface{}) {
	c.Debug(fmt.Sprintf(format, args...))
}
func (c customLogger) Info(msg string) { fmt.Printf("customLogger Info: %s\n", msg) }
func (c customLogger) Infof(format string, args ...interface{}) {
	c.Info(fmt.Sprintf(format, args...))
}
func (c customLogger) Warn(msg string) { fmt.Printf("customLogger Warn: %s\n", msg) }
func (c customLogger) Warnf(format string, args ...interface{}) {
	c.Warn(fmt.Sprintf(format, args...))
}
func (c customLogger) Error(msg string) { fmt.Printf("customLogger Error: %s\n", msg) }
func (c customLogger) Errorf(format string, args ...interface{}) {
	c.Error(fmt.Sprintf(format, args...))
}

// customLoggerFactory satisfies the interface logging.LoggerFactory
// This allows us to create different loggers per subsystem. So we can
// add custom behavior
type customLoggerFactory struct {
}

//实现了Logger接口，从而自定义Logger形式
//customLoggerFactory实现了LoggerFactory接口
//customLogger实现了LeveledLogger接口
func (c customLoggerFactory) NewLogger(subsystem string) logging.LeveledLogger {
	fmt.Printf("Creating logger for %s \n", subsystem)
	return customLogger{}
}

func main() {
	sdpChan := signal.HTTPSDPServer()

	// Everything below is the Pion WebRTC API, thanks for using it ❤️.
	offer := webrtc.SessionDescription{}
	signal.Decode(<-sdpChan, &offer)
	fmt.Println("")

	// Since we are answering use PayloadTypes declared by offerer
	mediaEngine := webrtc.MediaEngine{}
	//根据具体sdp涉及到的audio/video，注册对应的编解码器
	err := mediaEngine.PopulateFromSDP(offer)
	if err != nil {
		panic(err)
	}

	//使用自定义logger
	s := webrtc.SettingEngine{
		LoggerFactory: customLoggerFactory{},
	}

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithSettingEngine(s))

	peerConnectionConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}

	// Allow us to receive 1 video track
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	localTrackChan := make(chan *webrtc.Track)
	// Set a handler for when a new remote track starts, this just distributes all our packets
	// to connected peers
	//该peerConnection的远端为Publish端，本地端监听远端的OnTrack事件，当远端开始一个track发送时，即触发该回调
	//回调的内容为新建一个local track(如remote track绑定)，并将待发送的RTPSender都添加至local track的activeSenders中
	//一旦从remote track的receiver收到数据，则将数据分发给各个Pub端，即local track的activeSenders对应的RTPSender对象
	peerConnection.OnTrack(func(remoteTrack *webrtc.Track, receiver *webrtc.RTPReceiver) {
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		// This can be less wasteful by processing incoming RTCP events, then we would emit a NACK/PLI when a viewer requests it
		fmt.Printf("OnTrack Handler: track_id: %s, track_ssrc: %d, track_label: %s, \r" +
			"eceiver_track_id: %s, receiver_track_ssrc: %d, receiver_track_label: %s\n",
		remoteTrack.ID(), remoteTrack.SSRC(), remoteTrack.Label(), receiver.Track().ID(),
		receiver.Track().SSRC(), receiver.Track().Label())
		go func() {
			//每3s请求一次关键帧
			ticker := time.NewTicker(rtcpPLIInterval)
			for range ticker.C {
				if rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: remoteTrack.SSRC()}}); rtcpSendErr != nil {
					fmt.Println(rtcpSendErr)
				}
			}
		}()

		// Create a local track, all our SFU clients will be fed via this track
		localTrack, newTrackErr := peerConnection.NewTrack(remoteTrack.PayloadType(), remoteTrack.SSRC(), "video", "pion")
		if newTrackErr != nil {
			panic(newTrackErr)
		}
		localTrackChan <- localTrack
		fmt.Printf("OnTrack Handler -> NewLocalTrack: track_id: %s, track_ssrc: %d, track_label: %s\n",
			localTrack.ID(), localTrack.SSRC(), localTrack.Label())
		rtpBuf := make([]byte, 1400)
		for {
			//从Publisher端读取数据
			i, readErr := remoteTrack.Read(rtpBuf)
			if readErr != nil {
				panic(readErr)
			}
			// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
			//分发给Subscribers
			/*
			TODO:
				这里涉及到track结构体，track结构体有一个receiver和若干个activeSenders，
				每次将该track add到某个peerConnection，其实就是将该peerConnection的RTPSender
				添加到该track的activeSenders的过程，最后当该track的receiver收到数据时，
				会将其分发给所有的activeSenders。
				而一个MediaStream包含若干个track，其主要完成track同步，如音视频track的时间戳同步等。

			 */
			if _, err = localTrack.Write(rtpBuf[:i]); err != nil && err != io.ErrClosedPipe {
				panic(err)
			}
		}
	})

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	// Get the LocalDescription and take it to base64 so we can paste in browser
	fmt.Println(signal.Encode(answer))

	localTrack := <-localTrackChan
	for {
		fmt.Println("")
		fmt.Println("Curl an base64 SDP to start sendonly peer connection")
		//每次收到一个Subscriber的sdp，就建立一个peerConnection，并将localTrack添加到该peerConnection，即发送该Track给对端Subscriber
		recvOnlyOffer := webrtc.SessionDescription{}
		signal.Decode(<-sdpChan, &recvOnlyOffer)

		// Create a new PeerConnection
		peerConnection, err := api.NewPeerConnection(peerConnectionConfig)
		if err != nil {
			panic(err)
		}

		_, err = peerConnection.AddTrack(localTrack)
		if err != nil {
			panic(err)
		}

		// Set the remote SessionDescription
		err = peerConnection.SetRemoteDescription(recvOnlyOffer)
		if err != nil {
			panic(err)
		}

		// Create answer
		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			panic(err)
		}

		// Sets the LocalDescription, and starts our UDP listeners
		err = peerConnection.SetLocalDescription(answer)
		if err != nil {
			panic(err)
		}

		// Get the LocalDescription and take it to base64 so we can paste in browser
		fmt.Println(signal.Encode(answer))
	}
}
