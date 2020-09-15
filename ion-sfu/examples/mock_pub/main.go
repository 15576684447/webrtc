// Package pub-from-browser contains an example of publishing a stream to
// an ion-sfu instance from the browser.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"webrtc/webrtc"
	"webrtc/webrtc/ion-sfu/pkg/log"

	"google.golang.org/grpc"
	sfu "webrtc/webrtc/ion-sfu/pkg/proto"
)

const (
	address = "localhost:50051"
)

func main() {
	addr := flag.String("address", ":50000", "Address to host the HTTP server on.")
	flag.Parse()
	pubLogger := log.InitLogger("mock_pub")
	// Exchange the offer/answer via HTTP
	offerChan, answerChan := mustSignalViaHTTP(*addr)
	// Wait for the remote SessionDescription
	pubOffer := <-offerChan
	pubLogger.Debugf("pub receive offer: %+v\n", pubOffer)

	// Set up a connection to the server.
	conn, err := grpc.Dial(address, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		pubLogger.Errorf("did not connect: %v", err)
		return
	}
	defer conn.Close()
	c := sfu.NewSFUClient(conn)
	if err != nil {
		pubLogger.Errorf("error decoding pub offer")
		return
	}

	ctx := context.Background()
	stream, err := c.Publish(ctx)

	if err != nil {
		pubLogger.Errorf("Error publishing response: %v", err)
		return
	}

	err = stream.Send(&sfu.PublishRequest{
		Rid: "default",
		Payload: &sfu.PublishRequest_Connect{
			Connect: &sfu.Connect{
				Description: &sfu.SessionDescription{
					Type: pubOffer.Type.String(),
					Sdp:  []byte(pubOffer.SDP),
				},
			},
		},
	})

	if err != nil {
		pubLogger.Errorf("Error sending connect request: %v", err)
		return
	}

	for {
		res, err := stream.Recv()
		if err == io.EOF {
			// WebRTC Transport closed
			pubLogger.Debugf("WebRTC Transport Closed\n")
			return
		}

		if err != nil {
			pubLogger.Errorf("Error receiving publish response: %v", err)
			return
		}

		switch payload := res.Payload.(type) {
		case *sfu.PublishReply_Connect:
			// Output the mid and answer in base64 so we can paste it in browser
			pubLogger.Debugf("\npub mid: %s", res.Mid)
			answer := webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  string(payload.Connect.Description.Sdp),
			}
			answerChan <- answer
			pubLogger.Debugf("pub response answer: %+v\n", answer)
		}
	}
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