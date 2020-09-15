package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"github.com/pion/sdp/v2"
	"net/http"
	"webrtc/webrtc"
	"webrtc/webrtc/ion-sfu/pkg/log"
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

	codecMap := map[uint8]*webrtc.RTPCodec{
		// Opus
		webrtc.DefaultPayloadTypeOpus: webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000),
		109:                           webrtc.NewRTPOpusCodec(109, 48000),
		// VP8
		webrtc.DefaultPayloadTypeVP8: webrtc.NewRTPVP8CodecExt(webrtc.DefaultPayloadTypeVP8, 90000, rtcpfb, ""),
		120:                          webrtc.NewRTPVP8CodecExt(120, 90000, rtcpfb, ""),
		// VP9
		webrtc.DefaultPayloadTypeVP9: webrtc.NewRTPVP9Codec(webrtc.DefaultPayloadTypeVP9, 90000),
		121:                          webrtc.NewRTPVP9Codec(121, 90000),
	}
	for _, codec := range codecMap {
		mediaEngine.RegisterCodec(codec)
	}

	settingEngine.SetTrickle(false)
	settingEngine.DetachDataChannels()
	settingEngine.SetEphemeralUDPPortRange(50000, 60000)

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