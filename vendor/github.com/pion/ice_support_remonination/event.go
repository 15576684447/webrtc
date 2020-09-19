package ice_support_remonination

// EventCallback callback function
type EventCallback func(e Event)

// Event define
type Event int

// Event
const (
	ReceiveRequest Event = iota
	ReceiveSuccessResponse
	ReceiveErrorResponse

	SendRequest
	SendSuccessResponse
	SendErrorResponse

	SetSelectedPair
)

type SwitchReason int

const (
	NoReason SwitchReason = iota
	FirstConnectedReason
	SelectedDisconnectedReason
	HighPriorityConnectedReason
	RemoteSwitchReason
)

func (c SwitchReason) String() string {
	switch c {
	case NoReason:
		return "No reason"
	case FirstConnectedReason:
		return "First pair connected"
	case SelectedDisconnectedReason:
		return "Selected pair was disconnected"
	case HighPriorityConnectedReason:
		return "High priority pair was connected"
	case RemoteSwitchReason:
		return "Remote agent require switch"
	default:
		return "Invalid"
	}
}
