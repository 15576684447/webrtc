package webrtc

// RTPSendParameters contains the RTP stack settings used by receivers
type RTPSendParameters struct {
	DisableEncrypt bool
	Encodings      RTPEncodingParameters
}
