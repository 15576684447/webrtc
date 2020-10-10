package webrtc

// RTPReceiveParameters contains the RTP stack settings used by receivers
type RTPReceiveParameters struct {
	DisableEncrypt bool
	Encodings      RTPDecodingParameters
}
