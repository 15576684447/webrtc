package webrtc

// DTLSParameters holds information relating to DTLS configuration.
type DTLSParameters struct {
	DisableEncrypt bool
	Role           DTLSRole          `json:"role"`
	Fingerprints   []DTLSFingerprint `json:"fingerprints"`
}
