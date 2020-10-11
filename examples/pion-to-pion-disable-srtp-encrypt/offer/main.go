package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"time"
	"webrtc/webrtc"
	"webrtc/webrtc/examples/internal/signal"
	"webrtc/webrtc/pkg/media"
	"webrtc/webrtc/pkg/media/ivfreader"
)

func main() {
	addr := flag.String("address", "127.0.0.1:50000", "Address that the HTTP server is hosted on.")
	flag.Parse()

	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	m := webrtc.MediaEngine{}
	s := webrtc.SettingEngine{}
	m.RegisterDefaultCodecs()
	s.DisableSRTPEncrypt(true)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(s))
	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	// Create a video track
	rand.Seed(time.Now().Unix())
	videoSsrc := rand.Uint32()
	videoTrack, err := peerConnection.NewTrack(webrtc.DefaultPayloadTypeVP8, videoSsrc, "video", "pion")
	if err != nil {
		panic(err)
	}
	if _, err = peerConnection.AddTrack(videoTrack); err != nil {
		panic(err)
	}

	peerConnection.OnTrack(func(track *webrtc.Track, receiver *webrtc.RTPReceiver) {
		receiveTrack(track)
	})

	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(context.Background())
	go func() {
		// Open a IVF file and start reading using our IVFReader
		file, ivfErr := os.Open("output.ivf")
		if ivfErr != nil {
			panic(ivfErr)
		}

		ivf, header, ivfErr := ivfreader.NewWith(file)
		if ivfErr != nil {
			panic(ivfErr)
		}

		// Wait for connection established
		<-iceConnectedCtx.Done()

		// Send our video file frame at a time. Pace our sending so we send it at the same speed it should be played back as.
		// This isn't required since the video is timestamped, but we will such much higher loss if we send all at once.
		sleepTime := time.Millisecond * time.Duration((float32(header.TimebaseNumerator)/float32(header.TimebaseDenominator))*1000)
		for {
			frame, _, ivfErr := ivf.ParseNextFrame()
			if ivfErr == io.EOF {
				fmt.Printf("All frames parsed and sent")
				os.Exit(0)
			}

			if ivfErr != nil {
				panic(ivfErr)
			}

			time.Sleep(sleepTime)
			if ivfErr = videoTrack.WriteSample(media.Sample{Data: frame, Samples: 90000}); ivfErr != nil {
				panic(ivfErr)
			}
		}
	}()

	// Create a datachannel with label 'data'
	dataChannel, err := peerConnection.CreateDataChannel("data", nil)
	if err != nil {
		panic(err)
	}

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			iceConnectedCtxCancel()
		}
	})

	// Register channel opening handling
	dataChannel.OnOpen(func() {
		fmt.Printf("Data channel '%s'-'%d' open. Random messages will now be sent to any connected DataChannels every 5 seconds\n", dataChannel.Label(), dataChannel.ID())

		for range time.NewTicker(5 * time.Second).C {
			message := signal.RandSeq(15)
			fmt.Printf("Sending '%s'\n", message)

			// Send the message as text
			sendTextErr := dataChannel.SendText(message)
			if sendTextErr != nil {
				panic(sendTextErr)
			}
		}
	})

	// Register text message handling
	dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		fmt.Printf("Message from DataChannel '%s': '%s'\n", dataChannel.Label(), string(msg.Data))
	})

	// Create an offer to send to the browser
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}
	fmt.Printf("send offer: %s\n", offer.SDP)
	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(offer)
	if err != nil {
		panic(err)
	}

	// Exchange the offer for the answer
	answer := mustSignalViaHTTP(offer, *addr)
	fmt.Printf("get answer: %s\n", answer.SDP)

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

func receiveTrack(remoteTrack *webrtc.Track) {
	mediaType := "unknownMediaType"
	if remoteTrack.Kind() == webrtc.RTPCodecTypeAudio {
		mediaType = "audio"
	} else if remoteTrack.Kind() == webrtc.RTPCodecTypeVideo {
		mediaType = "video"
	}
	payloadType := remoteTrack.PayloadType()
	fmt.Printf("On %s track\n", mediaType)
	totalPktNum, totalPktBytes, avrPktBytes := 0, 0, 0
	for {
		rtpPacket, err := remoteTrack.ReadRTP()
		if err != nil {
			panic(err)
		}
		totalPktNum++
		totalPktBytes += len(rtpPacket.Raw)
		if totalPktNum%50 == 0 {
			avrPktBytes = totalPktBytes / totalPktNum
			fmt.Printf("%s track, payload=%d, totalPktNum=%d, totalPktBytes=%d, avrPktBytes=%d\n", mediaType, payloadType, totalPktNum, totalPktBytes, avrPktBytes)
		}

	}
}
