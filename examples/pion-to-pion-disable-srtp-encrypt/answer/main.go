package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"time"
	"webrtc/webrtc"
	"webrtc/webrtc/examples/internal/signal"
)

func main() {
	addr := flag.String("address", ":50000", "Address to host the HTTP server on.")
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
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			receiveTrack(track, videoTrack, videoSsrc)
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())
	})

	// Register data channel creation handling
	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		fmt.Printf("New DataChannel %s %d\n", d.Label(), d.ID())

		// Register channel opening handling
		d.OnOpen(func() {
			fmt.Printf("Data channel '%s'-'%d' open. Random messages will now be sent to any connected DataChannels every 5 seconds\n", d.Label(), d.ID())

			for range time.NewTicker(5 * time.Second).C {
				message := signal.RandSeq(15)
				fmt.Printf("Sending '%s'\n", message)

				// Send the message as text
				sendTextErr := d.SendText(message)
				if sendTextErr != nil {
					panic(sendTextErr)
				}
			}
		})

		// Register text message handling
		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			fmt.Printf("Message from DataChannel '%s': '%s'\n", d.Label(), string(msg.Data))
		})
	})

	// Exchange the offer/answer via HTTP
	offerChan, answerChan := mustSignalViaHTTP(*addr)

	// Wait for the remote SessionDescription
	offer := <-offerChan

	fmt.Printf("get offer: %s\n", offer.SDP)

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

	// Send the answer
	answerChan <- answer

	fmt.Printf("send answer: %s\n", answer.SDP)

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

func receiveTrack(remoteTrack *webrtc.Track, localTrack *webrtc.Track, targetSsrc uint32) {
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
		//发送到peer
		//将rtpPkt返回给peer端时，需要修改其中的ssrc为协商的ssrc，否则对端不认识该ssrc对应的媒体
		rtpPacket.SSRC = targetSsrc
		binary.BigEndian.PutUint32(rtpPacket.Raw[8:12], targetSsrc)
		if err := localTrack.WriteRTP(rtpPacket); err != nil {
			panic(err)
		}

	}
}
