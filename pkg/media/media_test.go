package media_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"webrtc/webrtc/pkg/media"
)

func TestNSamples(t *testing.T) {
	assert.Equal(t, media.NSamples(20*time.Millisecond, 48000), uint32(48000*0.02))
}
