package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/pion/sdp/v2"
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
	addr := flag.String("address", "127.0.0.1:50000", "Address that the HTTP server is hosted on.")
	flag.Parse()
	clientLogger := log.InitLogger("mock_client")
	// Prepare the configuration
	config := webrtc.Configuration{}
	mediaEngine := webrtc.MediaEngine{}
	settingEngine := webrtc.SettingEngine{}

	rtcpfb := []webrtc.RTCPFeedback{
		{Type: webrtc.TypeRTCPFBGoogREMB},
		{Type: webrtc.TypeRTCPFBCCM},
		{Type: webrtc.TypeRTCPFBNACK},
		{Type: "nack pli"},
	}
	//TODO:codecMap定义了codec类型与其代号的映射关系，并将其Register到mediaEngine中
	codecMap := map[uint8]*webrtc.RTPCodec{
		// Opus
		webrtc.DefaultPayloadTypeOpus: webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000),
		// H264
		webrtc.DefaultPayloadTypeH264: webrtc.NewRTPH264CodecExt(webrtc.DefaultPayloadTypeH264, 90000, rtcpfb, IOSH264Fmtp),
		97:                            webrtc.NewRTPH264CodecExt(97, 90000, rtcpfb, FireFoxH264Fmtp97),
	}
	for _, codec := range codecMap {
		mediaEngine.RegisterCodec(codec)
	}

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

	//TODO:在newTrack时，涉及到的codec的payload值将会从之前Register的列表中获取
	// 所以RegisterCodec可以按需注册，当然也可以RegisterDefaultCodecs注册所有默认的codec

	// Create a video track
	videoTrack, addTrackErr := peerConnection.NewTrack(getPayloadType(mediaEngine, webrtc.RTPCodecTypeVideo, "H264"), rand.Uint32(), "video", "bytertc")
	if addTrackErr != nil {
		panic(addTrackErr)
	}
	if _, addTrackErr = peerConnection.AddTrack(videoTrack); err != nil {
		panic(addTrackErr)
	}

	// Create a audio track
	audioTrack, addTrackErr := peerConnection.NewTrack(getPayloadType(mediaEngine, webrtc.RTPCodecTypeAudio, "opus"), rand.Uint32(), "audio", "bytertc")
	if addTrackErr != nil {
		panic(addTrackErr)
	}
	if _, addTrackErr = peerConnection.AddTrack(audioTrack); err != nil {
		panic(addTrackErr)
	}

	// Create an offer to send to the browser
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}
	clientLogger.Debugf("\nclient original offer: %+v\n", offer)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(offer)
	if err != nil {
		panic(err)
	}

	// Exchange the offer for the answer
	answer := mustSignalViaHTTP(offer, *addr)

	answerSdp := sdp.SessionDescription{}
	err = answerSdp.Unmarshal([]byte(answer.SDP))
	if err != nil {
		clientLogger.Debugf("client->answer: err=%v sdp=%v", err, offer)
		return
	}

	clientLogger.Debugf("answer: %+v\n", answer)
	// Apply the answer as the remote description
	err = peerConnection.SetRemoteDescription(answer)
	if err != nil {
		panic(err)
	}

	// Block forever
	select {}
}


// mustSignalViaHTTP exchange the SDP offer and answer using an HTTP Post request.
func mustSignalViaHTTP(offer webrtc.SessionDescription, address string) webrtc.SessionDescription {
	b := new(bytes.Buffer)
	err := json.NewEncoder(b).Encode(offer)
	if err != nil {
		panic(err)
	}

	resp, err := http.Post("http://"+address, "application/json; charset=utf-8", b)
	if err != nil {
		panic(err)
	}
	defer func() {
		closeErr := resp.Body.Close()
		if closeErr != nil {
			panic(closeErr)
		}
	}()

	var answer webrtc.SessionDescription
	err = json.NewDecoder(resp.Body).Decode(&answer)
	if err != nil {
		panic(err)
	}

	return answer
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