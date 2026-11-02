package oms

// IsTerminal reports whether no further events can change an order in this state.
func (s OrdStatus) IsTerminal() bool {
	switch s {
	case StatusFilled, StatusCanceled, StatusRejected, StatusExpired:
		return true
	}
	return false
}

var transitions = map[OrdStatus][]OrdStatus{
	StatusPendingNew:      {StatusNew, StatusRejected},
	StatusNew:             {StatusPartiallyFilled, StatusFilled, StatusCanceled, StatusExpired, StatusPendingCancel, StatusPendingReplace},
	StatusPartiallyFilled: {StatusPartiallyFilled, StatusFilled, StatusCanceled, StatusExpired, StatusPendingCancel, StatusPendingReplace},
	StatusPendingCancel:   {StatusCanceled, StatusNew, StatusPartiallyFilled, StatusFilled},
	StatusPendingReplace:  {StatusNew, StatusPartiallyFilled, StatusFilled, StatusCanceled},
}

// CanTransition reports whether an order may move from one status to another.
func CanTransition(from, to OrdStatus) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}
