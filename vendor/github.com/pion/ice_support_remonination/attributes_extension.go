package ice_support_remonination

import (
	"errors"

	"github.com/pion/stun"
)

//Attribute of stun.AttrType extension
const (
	AttrNomination stun.AttrType = 0xC001 // NOMINATION
	AttrNetwork    stun.AttrType = 0xC057 // NETWORK_INFO
)

const (
	nominationSize = 4 // 32 bit
)

//NominationAttr  represent nomination count
type NominationAttr struct {
	nomination uint32
}

// ErrNominationMismatch means that computed nomination differs from expected.
var ErrNominationMismatch = errors.New("nomination check failed")

// Nomination is shorthand for NominationAttr.
//
// Example:
//
//  m := New()
//  Nomination.AddTo(m)
var Nomination NominationAttr

// AddTo adds fingerprint to message.
func (n NominationAttr) AddTo(m *stun.Message) error {
	// length in header should include size of nomination attribute
	b := make([]byte, nominationSize)
	bin.PutUint32(b, n.nomination)
	m.Add(AttrNomination, b)
	return nil
}
