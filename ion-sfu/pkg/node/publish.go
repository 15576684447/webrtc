package sfu

import (
	"fmt"
	"strconv"

	"github.com/lucsky/cuid"
	"github.com/pion/sdp/v2"
	"webrtc/webrtc"

	"webrtc/webrtc/ion-sfu/pkg/log"
	"webrtc/webrtc/ion-sfu/pkg/rtc"
	transport "webrtc/webrtc/ion-sfu/pkg/rtc/transport"

	pb "webrtc/webrtc/ion-sfu/pkg/proto"
)

func getPubCodecs(sdp sdp.SessionDescription) ([]uint8, error) {
	allowedCodecs := make([]uint8, 0)
	for _, md := range sdp.MediaDescriptions {
		if md.MediaName.Media != "audio" && md.MediaName.Media != "video" {
			continue
		}

		for _, format := range md.MediaName.Formats {
			pt, err := strconv.Atoi(format)
			if err != nil {
				return nil, fmt.Errorf("format parse error")
			}

			if pt < 0 || pt > 255 {
				return nil, fmt.Errorf("payload type out of range: %d", pt)
			}

			payloadType := uint8(pt)
			//从Media的属性中获取payloadType对应的codecName
			/*
			如
			m=audio 9 UDP/TLS/RTP/SAVPF 111 103 104 9 0 8 106 105 13 126
			...
			a=rtpmap:111 opus/48000/2
			a=rtcp-fb:111 transport-cc
			a=fmtp:111
			a=rtpmap:103 ISAC/16000
			...
			 */
			payloadCodec, err := sdp.GetCodecForPayloadType(payloadType)
			if err != nil {
				return nil, fmt.Errorf("could not find codec for payload type %d", payloadType)
			}
			log.Logger.Debugf("getPubCodecs: MediaName=%s, CodecName=%s\n", md.MediaName.Media, payloadCodec.Name)
			if md.MediaName.Media == "audio" {
				if payloadCodec.Name == webrtc.Opus {
					allowedCodecs = append(allowedCodecs, payloadType)
					break
				}
			} else {
				// skip 126 for pub, chrome sub decode will fail when H264 playload type is 126
				if payloadCodec.Name == webrtc.H264 && payloadType == 126 {
					continue
				}
				allowedCodecs = append(allowedCodecs, payloadType)
				break
			}
		}
	}

	return allowedCodecs, nil
}

func (s *server) publish(payload *pb.PublishRequest_Connect) (*transport.WebRTCTransport, *pb.PublishReply_Connect, error) {
	mid := cuid.New()
	offer := sdp.SessionDescription{}
	err := offer.Unmarshal(payload.Connect.Description.Sdp)

	if err != nil {
		log.Logger.Debugf("publish->connect: err=%v sdp=%v", err, offer)
		return nil, nil, errSdpParseFailed
	}
	log.Logger.Debugf("publish get offer->Attributes %+v\n", offer.Attributes)
	log.Logger.Debugf("publish get offer->MediaDescriptions\n")
	for index, md := range offer.MediaDescriptions {
		log.Logger.Debugf("[%d]: %+v\n", index, md)
	}

	rtcOptions := transport.RTCOptions{
		Publish: true,
	}
	//获取pub端有效的codec参数
	codecs, err := getPubCodecs(offer)

	if err != nil {
		log.Logger.Debugf("publish->connect: err=%v", err)
		return nil, nil, errSdpParseFailed
	}

	rtcOptions.Codecs = codecs
	pub := transport.NewWebRTCTransport(mid, rtcOptions)
	if pub == nil {
		return nil, nil, errWebRTCTransportInitFailed
	}

	router := rtc.AddRouter(mid)

	answer, err := pub.Answer(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: string(payload.Connect.Description.Sdp),
	}, rtcOptions)

	if err != nil {
		log.Logger.Debugf("publish->connect: error creating answer %v", err)
		return nil, nil, errWebRTCTransportAnswerFailed
	}

	log.Logger.Debugf("publish->connect: answer => %v", answer)

	router.AddPub(pub)

	return pub, &pb.PublishReply_Connect{
		Connect: &pb.Connect{
			Description: &pb.SessionDescription{
				Type: answer.Type.String(),
				Sdp:  []byte(answer.SDP),
			},
		},
	}, nil
}
