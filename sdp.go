// +build !js

package webrtc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pion/logging"
	"github.com/pion/sdp/v2"
)

type trackDetails struct {
	mid   string
	kind  RTPCodecType
	label string
	id    string
	ssrc  uint32
}

// extract all trackDetails from an SDP.
func trackDetailsFromSDP(log logging.LeveledLogger, s *sdp.SessionDescription) map[uint32]trackDetails {
	incomingTracks := map[uint32]trackDetails{}
	rtxRepairFlows := map[uint32]bool{}

	for _, media := range s.MediaDescriptions {
		// Plan B can have multiple tracks in a signle media section
		trackLabel := ""
		trackID := ""

		// If media section is recvonly or inactive skip
		if _, ok := media.Attribute(sdp.AttrKeyRecvOnly); ok {
			continue
		} else if _, ok := media.Attribute(sdp.AttrKeyInactive); ok {
			continue
		}

		midValue := getMidValue(media)
		if midValue == "" {
			continue
		}

		for _, attr := range media.Attributes {
			codecType := NewRTPCodecType(media.MediaName.Media)
			if codecType == 0 {
				continue
			}
			/*
			a=ssrc-group:FID 3463951252 1461041037
			//在webrtc中，重传包和正常包ssrc是不同的，上一行中前一个是正常rtp包的ssrc,后一个是重传包的ssrc

			a=ssrc:3463951252 cname:sTjtznXLCNH7nbRw
			//cname用来标识一个数据源，ssrc当发生冲突时可能会发生变化，但是cname不会发生变化，也会出现在rtcp包中SDEC中，用于音视频同步
			a=ssrc:3463951252 msid:h1aZ20mbQB0GSsq0YxLfJmiYWE9CBfGch97C ead4b4e9-b650-4ed5-86f8-6f5f5806346d
			//定义了ssrc和WebRTC中的MediaStream,Track之间的关系，msid后面第一个属性是stream-id,第二个是track-id
			a=ssrc:3463951252 mslabel:h1aZ20mbQB0GSsq0YxLfJmiYWE9CBfGch97C
			a=ssrc:3463951252 label:ead4b4e9-b650-4ed5-86f8-6f5f5806346d

			a=ssrc:1461041037 cname:sTjtznXLCNH7nbRw
			a=ssrc:1461041037 msid:h1aZ20mbQB0GSsq0YxLfJmiYWE9CBfGch97C ead4b4e9-b650-4ed5-86f8-6f5f5806346d
			a=ssrc:1461041037 mslabel:h1aZ20mbQB0GSsq0YxLfJmiYWE9CBfGch97C
			a=ssrc:1461041037 label:ead4b4e9-b650-4ed5-86f8-6f5f5806346d
			*/
			/*
			虽然PlanB模式和UnifiedPlan模式对于流组织方式不太一样，但是其ssrc属性是类似的，可以从ssrc属性中得知sdp中包含的所有track信息
			 */
			switch attr.Key {
			case sdp.AttrKeySSRCGroup://ssrc-group
				split := strings.Split(attr.Value, " ")
				if split[0] == sdp.SemanticTokenFlowIdentification {
					// Add rtx ssrcs to blacklist, to avoid adding them as tracks
					// Essentially lines like `a=ssrc-group:FID 2231627014 632943048` are processed by this section
					// as this declares that the second SSRC (632943048) is a rtx repair flow (RFC4588) for the first
					// (2231627014) as specified in RFC5576
					//第二个ssrc是第一个的重传ssrc，不能作为ssrc track
					if len(split) == 3 {
						_, err := strconv.ParseUint(split[1], 10, 32)
						if err != nil {
							log.Warnf("Failed to parse SSRC: %v", err)
							continue
						}
						rtxRepairFlow, err := strconv.ParseUint(split[2], 10, 32)
						if err != nil {
							log.Warnf("Failed to parse SSRC: %v", err)
							continue
						}
						//标记重传ssrc并从track中移除
						rtxRepairFlows[uint32(rtxRepairFlow)] = true
						delete(incomingTracks, uint32(rtxRepairFlow)) // Remove if rtx was added as track before
					}
				}

			// Handle `a=msid:<stream_id> <track_label>` for Unified plan. The first value is the same as MediaStream.id
			// in the browser and can be used to figure out which tracks belong to the same stream. The browser should
			// figure this out automatically when an ontrack event is emitted on RTCPeerConnection.
			//a=msid:<stream_id> <track_label>第一个是streamid，第二个是trackid
			case sdp.AttrKeyMsid://msid
				split := strings.Split(attr.Value, " ")
				if len(split) == 2 {
					trackLabel = split[0]
					trackID = split[1]
				}
			//a=ssrc:3463951252 msid:h1aZ20mbQB0GSsq0YxLfJmiYWE9CBfGch97C ead4b4e9-b650-4ed5-86f8-6f5f5806346d
			//a=ssrc:<ssrc_id> msid:<stream_id> <track_label>
			//监测时，排除重传ssrc，将真实的ssrc加入incomingTracks中(需要去重)
			case sdp.AttrKeySSRC://ssrc
				split := strings.Split(attr.Value, " ")
				ssrc, err := strconv.ParseUint(split[0], 10, 32)
				if err != nil {
					log.Warnf("Failed to parse SSRC: %v", err)
					continue
				}
				//排除重传ssrc
				if rtxRepairFlow := rtxRepairFlows[uint32(ssrc)]; rtxRepairFlow {
					continue // This ssrc is a RTX repair flow, ignore
				}
				//去重
				if existingValues, ok := incomingTracks[uint32(ssrc)]; ok && existingValues.label != "" && existingValues.id != "" {
					continue // This ssrc is already fully defined
				}

				if len(split) == 3 && strings.HasPrefix(split[1], "msid:") {
					trackLabel = split[1][len("msid:"):]
					trackID = split[2]
				}

				// Plan B might send multiple a=ssrc lines under a single m= section. This is also why a single trackDetails{}
				// is not defined at the top of the loop over s.MediaDescriptions.
				incomingTracks[uint32(ssrc)] = trackDetails{midValue, codecType, trackLabel, trackID, uint32(ssrc)}
			}
		}
	}

	return incomingTracks
}

func addCandidatesToMediaDescriptions(candidates []ICECandidate, m *sdp.MediaDescription, iceGatheringState ICEGatheringState) {
	appendCandidateIfNew := func(c sdp.ICECandidate, attributes []sdp.Attribute) {
		marshaled := c.Marshal()
		for _, a := range attributes {
			if marshaled == a.Value {
				return
			}
		}

		m.WithICECandidate(c)
	}

	for _, c := range candidates {
		sdpCandidate := iceCandidateToSDP(c)
		sdpCandidate.ExtensionAttributes = append(sdpCandidate.ExtensionAttributes, sdp.ICECandidateAttribute{Key: "generation", Value: "0"})
		sdpCandidate.Component = 1
		appendCandidateIfNew(sdpCandidate, m.Attributes)

		sdpCandidate.Component = 2
		appendCandidateIfNew(sdpCandidate, m.Attributes)
	}

	if iceGatheringState != ICEGatheringStateComplete {
		return
	}
	for _, a := range m.Attributes {
		if a.Key == "end-of-candidates" {
			return
		}
	}

	m.WithPropertyAttribute("end-of-candidates")
}

func addDataMediaSection(d *sdp.SessionDescription, midValue string, iceParams ICEParameters, candidates []ICECandidate, dtlsRole sdp.ConnectionRole, iceGatheringState ICEGatheringState) {
	media := (&sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   mediaSectionApplication,
			Port:    sdp.RangedPort{Value: 9},
			Protos:  []string{"DTLS", "SCTP"},
			Formats: []string{"5000"},
		},
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address: &sdp.Address{
				Address: "0.0.0.0",
			},
		},
	}).
		WithValueAttribute(sdp.AttrKeyConnectionSetup, dtlsRole.String()).
		WithValueAttribute(sdp.AttrKeyMID, midValue).
		WithPropertyAttribute(RTPTransceiverDirectionSendrecv.String()).
		WithPropertyAttribute("sctpmap:5000 webrtc-datachannel 1024").
		WithICECredentials(iceParams.UsernameFragment, iceParams.Password)

	addCandidatesToMediaDescriptions(candidates, media, iceGatheringState)
	d.WithMedia(media)
}

func addFingerprints(d *sdp.SessionDescription, c Certificate) error {
	fingerprints, err := c.GetFingerprints()
	if err != nil {
		return err
	}
	for _, fingerprint := range fingerprints {
		d.WithFingerprint(fingerprint.Algorithm, strings.ToUpper(fingerprint.Value))
	}
	return nil
}

func populateLocalCandidates(sessionDescription *SessionDescription, i *ICEGatherer, iceGatheringState ICEGatheringState) *SessionDescription {
	if sessionDescription == nil || i == nil {
		return sessionDescription
	}

	candidates, err := i.GetLocalCandidates()
	if err != nil {
		return sessionDescription
	}

	parsed := sessionDescription.parsed
	for _, m := range parsed.MediaDescriptions {
		addCandidatesToMediaDescriptions(candidates, m, iceGatheringState)
	}
	sdp, err := parsed.Marshal()
	if err != nil {
		return sessionDescription
	}

	return &SessionDescription{
		SDP:  string(sdp),
		Type: sessionDescription.Type,
	}
}

func addTransceiverSDP(d *sdp.SessionDescription, isPlanB bool, mediaEngine *MediaEngine, midValue string, iceParams ICEParameters, candidates []ICECandidate, dtlsRole sdp.ConnectionRole, iceGatheringState ICEGatheringState, transceivers ...*RTPTransceiver) (bool, error) {
	if len(transceivers) < 1 {
		return false, fmt.Errorf("addTransceiverSDP() called with 0 transceivers")
	}
	// Use the first transceiver to generate the section attributes
	t := transceivers[0]
	media := sdp.NewJSEPMediaDescription(t.kind.String(), []string{}).
		WithValueAttribute(sdp.AttrKeyConnectionSetup, dtlsRole.String()).
		WithValueAttribute(sdp.AttrKeyMID, midValue).
		WithICECredentials(iceParams.UsernameFragment, iceParams.Password).
		WithPropertyAttribute(sdp.AttrKeyRTCPMux).
		WithPropertyAttribute(sdp.AttrKeyRTCPRsize)

	codecs := mediaEngine.GetCodecsByKind(t.kind)
	for _, codec := range codecs {
		media.WithCodec(codec.PayloadType, codec.Name, codec.ClockRate, codec.Channels, codec.SDPFmtpLine)

		for _, feedback := range codec.RTPCodecCapability.RTCPFeedback {
			media.WithValueAttribute("rtcp-fb", fmt.Sprintf("%d %s %s", codec.PayloadType, feedback.Type, feedback.Parameter))
			if feedback.Type == TypeRTCPFBTransportCC {
				media.WithTransportCCExtMap()
			}
		}
	}
	if len(codecs) == 0 {
		// Explicitly reject track if we don't have the codec
		d.WithMedia(&sdp.MediaDescription{
			MediaName: sdp.MediaName{
				Media:   t.kind.String(),
				Port:    sdp.RangedPort{Value: 0},
				Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
				Formats: []string{"0"},
			},
		})
		return false, nil
	}

	for _, mt := range transceivers {
		if mt.Sender() != nil && mt.Sender().track != nil {
			track := mt.Sender().track
			media = media.WithMediaSource(track.SSRC(), track.Label() /* cname */, track.Label() /* streamLabel */, track.ID())
			if !isPlanB {
				media = media.WithPropertyAttribute("msid:" + track.Label() + " " + track.ID())
				break
			}
		}
	}

	media = media.WithPropertyAttribute(t.Direction().String())

	addCandidatesToMediaDescriptions(candidates, media, iceGatheringState)
	d.WithMedia(media)

	return true, nil
}

type mediaSection struct {
	id           string
	transceivers []*RTPTransceiver
	data         bool
}

// populateSDP serializes a PeerConnections state into an SDP
func populateSDP(d *sdp.SessionDescription, isPlanB bool, isICELite bool, mediaEngine *MediaEngine, connectionRole sdp.ConnectionRole, candidates []ICECandidate, iceParams ICEParameters, mediaSections []mediaSection, iceGatheringState ICEGatheringState) (*sdp.SessionDescription, error) {
	var err error

	bundleValue := "BUNDLE"
	bundleCount := 0
	appendBundle := func(midValue string) {
		bundleValue += " " + midValue
		bundleCount++
	}

	for _, m := range mediaSections {
		if m.data && len(m.transceivers) != 0 {
			return nil, fmt.Errorf("invalid Media Section. Media + DataChannel both enabled")
		} else if !isPlanB && len(m.transceivers) > 1 {
			return nil, fmt.Errorf("invalid Media Section. Can not have multiple tracks in one MediaSection in UnifiedPlan")
		}

		shouldAddID := true
		if m.data {
			addDataMediaSection(d, m.id, iceParams, candidates, connectionRole, iceGatheringState)
		} else if shouldAddID, err = addTransceiverSDP(d, isPlanB, mediaEngine, m.id, iceParams, candidates, connectionRole, iceGatheringState, m.transceivers...); err != nil {
			return nil, err
		}

		if shouldAddID {
			appendBundle(m.id)
		}
	}

	if isICELite {
		// RFC 5245 S15.3
		d = d.WithValueAttribute(sdp.AttrKeyICELite, sdp.AttrKeyICELite)
	}
	return d.WithValueAttribute(sdp.AttrKeyGroup, bundleValue), nil
}

func getMidValue(media *sdp.MediaDescription) string {
	for _, attr := range media.Attributes {
		if attr.Key == "mid" {
			return attr.Value
		}
	}
	return ""
}

func descriptionIsPlanB(desc *SessionDescription) bool {
	if desc == nil || desc.parsed == nil {
		return false
	}

	detectionRegex := regexp.MustCompile(`(?i)^(audio|video|data)$`)
	for _, media := range desc.parsed.MediaDescriptions {
		if len(detectionRegex.FindStringSubmatch(getMidValue(media))) == 2 {
			return true
		}
	}
	return false
}

func getPeerDirection(media *sdp.MediaDescription) RTPTransceiverDirection {
	for _, a := range media.Attributes {
		if direction := NewRTPTransceiverDirection(a.Key); direction != RTPTransceiverDirection(Unknown) {
			return direction
		}
	}
	return RTPTransceiverDirection(Unknown)
}

func extractFingerprint(desc *sdp.SessionDescription) (string, string, error) {
	fingerprints := []string{}

	if fingerprint, haveFingerprint := desc.Attribute("fingerprint"); haveFingerprint {
		fingerprints = append(fingerprints, fingerprint)
	}

	for _, m := range desc.MediaDescriptions {
		if fingerprint, haveFingerprint := m.Attribute("fingerprint"); haveFingerprint {
			fingerprints = append(fingerprints, fingerprint)
		}
	}

	if len(fingerprints) < 1 {
		return "", "", ErrSessionDescriptionNoFingerprint
	}

	for _, m := range fingerprints {
		if m != fingerprints[0] {
			return "", "", ErrSessionDescriptionConflictingFingerprints
		}
	}

	parts := strings.Split(fingerprints[0], " ")
	if len(parts) != 2 {
		return "", "", ErrSessionDescriptionInvalidFingerprint
	}
	return parts[1], parts[0], nil
}

func extractICEDetails(desc *sdp.SessionDescription) (string, string, []ICECandidate, error) {
	candidates := []ICECandidate{}
	remotePwd := ""
	remoteUfrag := ""

	for _, m := range desc.MediaDescriptions {
		for _, a := range m.Attributes {
			switch {
			case a.IsICECandidate():
				sdpCandidate, err := a.ToICECandidate()
				if err != nil {
					return "", "", nil, err
				}

				candidate, err := newICECandidateFromSDP(sdpCandidate)
				if err != nil {
					return "", "", nil, err
				}

				candidates = append(candidates, candidate)
			case strings.HasPrefix(*a.String(), "ice-ufrag"):
				remoteUfrag = (*a.String())[len("ice-ufrag:"):]
			case strings.HasPrefix(*a.String(), "ice-pwd"):
				remotePwd = (*a.String())[len("ice-pwd:"):]
			}
		}
	}

	if remoteUfrag == "" {
		return "", "", nil, ErrSessionDescriptionMissingIceUfrag
	} else if remotePwd == "" {
		return "", "", nil, ErrSessionDescriptionMissingIcePwd
	}

	return remoteUfrag, remotePwd, candidates, nil
}

func haveApplicationMediaSection(desc *sdp.SessionDescription) bool {
	for _, m := range desc.MediaDescriptions {
		if m.MediaName.Media == mediaSectionApplication {
			return true
		}
	}

	return false
}
