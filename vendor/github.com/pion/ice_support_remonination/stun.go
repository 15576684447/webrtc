package ice

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/pion/stun"
)

// bin is shorthand for BigEndian.
var bin = binary.BigEndian

func getStunUsername(m *stun.Message) (string, error) {
	var username stun.Username
	if err := username.GetFrom(m); err != nil {
		return "", err
	}
	return username.String(), nil
}

func assertInboundUsername(m *stun.Message, expectedUsername string) error {
	var username stun.Username
	if err := username.GetFrom(m); err != nil {
		return err
	}

	if !strings.HasPrefix(string(username), expectedUsername) {
		return fmt.Errorf("username mismatch expected(%s) actual(%s)", expectedUsername, string(username))
	}

	return nil
}

func assertInboundMessageIntegrity(m *stun.Message, key []byte) error {
	messageIntegrityAttr := stun.MessageIntegrity(key)
	return messageIntegrityAttr.Check(m)
}
