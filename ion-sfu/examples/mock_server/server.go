package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"webrtc/webrtc"
	"webrtc/webrtc/ion-sfu/pkg/log"
)

const (
	IOSH264Fmtp       = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"
	FireFoxH264Fmtp97 = "profile-level-id=42e01f;level-asymmetry-allowed=1"
)

func main() {
	addr := flag.String("address", ":50000", "Address to host the HTTP server on.")
	flag.Parse()
	clientLogger := log.InitLogger("mock_client")

	// Exchange the offer/answer via HTTP
	offerChan, answerChan := mustSignalViaHTTP(*addr)
	// Wait for the remote SessionDescription
	offer := <-offerChan
	clientLogger.Debugf("server receive offer: %+v\n", offer)

	// Prepare the configuration
	config := webrtc.Configuration{}
	mediaEngine := webrtc.MediaEngine{}
	settingEngine := webrtc.SettingEngine{}
	/*
		rtcpfb := []webrtc.RTCPFeedback{
			{Type: webrtc.TypeRTCPFBGoogREMB},
			{Type: webrtc.TypeRTCPFBCCM},
			{Type: webrtc.TypeRTCPFBNACK},
			{Type: "nack pli"},
		}

		codecMap := map[uint8]*webrtc.RTPCodec{
			// Opus
			webrtc.DefaultPayloadTypeOpus: webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000),
			// H264
			webrtc.DefaultPayloadTypeH264: webrtc.NewRTPH264CodecExt(webrtc.DefaultPayloadTypeH264, 90000, rtcpfb, IOSH264Fmtp),
			97:                            webrtc.NewRTPH264CodecExt(97, 90000, rtcpfb, FireFoxH264Fmtp97),
		}
		for _, codec := range codecMap {
			mediaEngine.RegisterCodec(codec)
		}*/
	mediaEngine.RegisterDefaultCodecs()

	settingEngine.SetTrickle(false)
	settingEngine.SetEphemeralUDPPortRange(50000, 60000)
	settingEngine.LoggerFactory = log.CustomLoggerFactory{}

	webrtcApi := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithSettingEngine(settingEngine))
	peerConnection, err := webrtcApi.NewPeerConnection(config)
	if err != nil {
		clientLogger.Errorf("create peerConnection fail: %v\n", err)
	}

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		clientLogger.Debugf("ICE Connection State has changed: %s\n", connectionState.String())
	})

	peerConnection.OnTrack(func(track *webrtc.Track, receiver *webrtc.RTPReceiver) {
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		/*
			go func() {
				ticker := time.NewTicker(time.Second * 3)
				for range ticker.C {
					errSend := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: track.SSRC()}})
					if errSend != nil {
						fmt.Println(errSend)
					}
				}
			}()*/

		codec := track.Codec()
		if codec.Name == webrtc.Opus {
			fmt.Println("Got Opus track...")
			//socket.Socket.WriteTrackToSocket(track, true)
		} else if codec.Name == webrtc.H264 {
			fmt.Println("Got H264 track...")
			//socket.Socket.WriteTrackToSocket(track, false)
		}
	})

	// Create a video track
	videoTrack, addTrackErr := peerConnection.NewTrack(getPayloadType(mediaEngine, webrtc.RTPCodecTypeVideo, "H264"), rand.Uint32(), "video", "bytertc")
	if addTrackErr != nil {
		panic(addTrackErr)
	}
	if _, addTrackErr = peerConnection.AddTrack(videoTrack); addTrackErr != nil {
		panic(addTrackErr)
	}

	// Create a audio track
	audioTrack, addTrackErr := peerConnection.NewTrack(getPayloadType(mediaEngine, webrtc.RTPCodecTypeAudio, "opus"), rand.Uint32(), "audio", "bytertc")
	if addTrackErr != nil {
		panic(addTrackErr)
	}
	if _, addTrackErr = peerConnection.AddTrack(audioTrack); addTrackErr != nil {
		panic(addTrackErr)
	}

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	// Create an answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	answerChan <- answer

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	clientLogger.Debugf("answer: %+v\n", answer)

	// Block forever
	select {}
}

// mustSignalViaHTTP exchange the SDP offer and answer using an HTTP server.
func mustSignalViaHTTP(address string) (chan webrtc.SessionDescription, chan webrtc.SessionDescription) {
	offerOut := make(chan webrtc.SessionDescription)
	answerIn := make(chan webrtc.SessionDescription)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			http.Error(w, "Please send a request body", 400)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", http.MethodPost)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Please send a "+http.MethodPost+" request", 400)
			return
		}

		var offer webrtc.SessionDescription
		err := json.NewDecoder(r.Body).Decode(&offer)
		if err != nil {
			panic(err)
		}

		offerOut <- offer
		answer := <-answerIn

		err = json.NewEncoder(w).Encode(answer)
		if err != nil {
			panic(err)
		}
	})

	go func() {
		panic(http.ListenAndServe(address, nil))
	}()
	fmt.Println("Listening on", address)

	return offerOut, answerIn
}

// Since we are answering we need to match the remote PayloadType
func getPayloadType(m webrtc.MediaEngine, codecType webrtc.RTPCodecType, codecName string) uint8 {
	for _, codec := range m.GetCodecsByKind(codecType) {
		if codec.Name == codecName {
			return codec.PayloadType
		}
	}
	panic(fmt.Sprintf("Remote peer does not support %s", codecName))
}
