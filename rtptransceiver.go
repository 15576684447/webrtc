// +build !js

package webrtc

import (
	"fmt"
	"sync/atomic"
)

// RTPTransceiver represents a combination of an RTPSender and an RTPReceiver that share a common mid.
type RTPTransceiver struct {
	mid       atomic.Value // string
	sender    atomic.Value // *RTPSender
	receiver  atomic.Value // *RTPReceiver
	direction atomic.Value // RTPTransceiverDirection

	stopped bool
	kind    RTPCodecType
}

// Sender returns the RTPTransceiver's RTPSender if it has one
func (t *RTPTransceiver) Sender() *RTPSender {
	if v := t.sender.Load(); v != nil {
		return v.(*RTPSender)
	}

	return nil
}

func (t *RTPTransceiver) setSender(s *RTPSender) {
	t.sender.Store(s)
}

// Receiver returns the RTPTransceiver's RTPReceiver if it has one
func (t *RTPTransceiver) Receiver() *RTPReceiver {
	if v := t.receiver.Load(); v != nil {
		return v.(*RTPReceiver)
	}

	return nil
}

// setMid sets the RTPTransceiver's mid. If it was already set, will return an error.
func (t *RTPTransceiver) setMid(mid string) error {
	if currentMid := t.Mid(); currentMid != "" {
		return fmt.Errorf("cannot change transceiver mid from: %s to %s", currentMid, mid)
	}
	t.mid.Store(mid)
	return nil
}

// Mid gets the Transceiver's mid value. When not already set, this value will be set in CreateOffer or CreateAnswer.
func (t *RTPTransceiver) Mid() string {
	if v := t.mid.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// Kind returns RTPTransceiver's kind.
func (t *RTPTransceiver) Kind() RTPCodecType {
	return t.kind
}

// Direction returns the RTPTransceiver's current direction
func (t *RTPTransceiver) Direction() RTPTransceiverDirection {
	return t.direction.Load().(RTPTransceiverDirection)
}

// Stop irreversibly stops the RTPTransceiver
func (t *RTPTransceiver) Stop() error {
	if t.Sender() != nil {
		if err := t.Sender().Stop(); err != nil {
			return err
		}
	}
	if t.Receiver() != nil {
		if err := t.Receiver().Stop(); err != nil {
			return err
		}
	}

	t.setDirection(RTPTransceiverDirectionInactive)
	return nil
}

func (t *RTPTransceiver) setReceiver(r *RTPReceiver) {
	t.receiver.Store(r)
}

func (t *RTPTransceiver) setDirection(d RTPTransceiverDirection) {
	t.direction.Store(d)
}

/*
TODO:
	设置RTPTransceiver的direction属性：
		如果添加的track不为nil，即使用Sender，则在direction中增加Send属性
		如果添加的track为nil，即清除Sender，则移除direction的Send属性
	RTPTransceiver的Sender可以被复用的前提：
		direction没有被设置Send属性，如果被设置，说明已经有track占用了Sender
		所以在移除其上的track时，需要同时将Send属性移除
 */
func (t *RTPTransceiver) setSendingTrack(track *Track) error {
	t.Sender().track = track
	if track == nil {
		t.setSender(nil)
	}
	/*
	RTPTransceiverDirectionInactive:未指定方向，如果只增加send属性，则变为RTPTransceiverDirectionSendonly
	如果只增加receive属性，则变为RTPTransceiverDirectionRecvonly
	如果同时增加send和receive，则变为RTPTransceiverDirectionSendrecv
	 */
	switch {
	//track不为nil，则增加send属性
	case track != nil && t.Direction() == RTPTransceiverDirectionRecvonly:
		t.setDirection(RTPTransceiverDirectionSendrecv)
	case track != nil && t.Direction() == RTPTransceiverDirectionInactive:
		t.setDirection(RTPTransceiverDirectionSendonly)
	//track为nil，则移除send属性
	case track == nil && t.Direction() == RTPTransceiverDirectionSendrecv:
		t.setDirection(RTPTransceiverDirectionRecvonly)
	case track == nil && t.Direction() == RTPTransceiverDirectionSendonly:
		t.setDirection(RTPTransceiverDirectionInactive)
	default:
		return fmt.Errorf("invalid state change in RTPTransceiver.setSending")
	}
	return nil
}

func findByMid(mid string, localTransceivers []*RTPTransceiver) (*RTPTransceiver, []*RTPTransceiver) {
	for i, t := range localTransceivers {
		if t.Mid() == mid {
			return t, append(localTransceivers[:i], localTransceivers[i+1:]...)
		}
	}

	return nil, localTransceivers
}

// Given a direction+type pluck a transceiver from the passed list
// if no entry satisfies the requested type+direction return a inactive Transceiver
func satisfyTypeAndDirection(remoteKind RTPCodecType, remoteDirection RTPTransceiverDirection, localTransceivers []*RTPTransceiver) (*RTPTransceiver, []*RTPTransceiver) {
	// Get direction order from most preferred to least
	//根据remote direction，估计local direction
	getPreferredDirections := func() []RTPTransceiverDirection {
		switch remoteDirection {
		case RTPTransceiverDirectionSendrecv:
			return []RTPTransceiverDirection{RTPTransceiverDirectionRecvonly, RTPTransceiverDirectionSendrecv}
		case RTPTransceiverDirectionSendonly:
			return []RTPTransceiverDirection{RTPTransceiverDirectionRecvonly, RTPTransceiverDirectionSendrecv}
		case RTPTransceiverDirectionRecvonly:
			return []RTPTransceiverDirection{RTPTransceiverDirectionSendonly, RTPTransceiverDirectionSendrecv}
		}
		return []RTPTransceiverDirection{}
	}
	//local Transceive和remote Transceiver direction相反，type相同
	//根据这个依据，确定local Transceive的direction和type
	//找出type和direction相符的，如果找到，则返回对应的Transceiver和剩余的Transceivers
	//否则返回nil和所有Transceivers
	for _, possibleDirection := range getPreferredDirections() {
		for i := range localTransceivers {
			t := localTransceivers[i]
			//之前根据Mid匹配过一次，未匹配的肯定是Mid为空的
			if t.Mid() == "" && t.kind == remoteKind && possibleDirection == t.Direction() {
				return t, append(localTransceivers[:i], localTransceivers[i+1:]...)
			}
		}
	}

	return nil, localTransceivers
}
