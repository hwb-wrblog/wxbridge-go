package workflow

import (
	"errors"

	"feiyu.com/wx/protobuf/wechat"
)

// MessageCursor is the exclusive end of the inbox. Acknowledgement and logout
// advance its base instead of reusing positions already issued to a consumer.
func (s *Session) MessageCursor() uint64 {
	return s.State.PendingBase + uint64(len(s.State.Pending))
}

func (s *Session) AckMessages(cursor uint64) (int, error) {
	if cursor > s.MessageCursor() {
		return 0, errors.New("acknowledgement cursor is beyond the saved inbox")
	}
	if cursor <= s.State.PendingBase {
		return 0, nil
	}
	count := int(cursor - s.State.PendingBase)
	previousBase, previousPending := s.State.PendingBase, s.State.Pending
	s.State.PendingBase = cursor
	// Allocate a new slice so acknowledged message bodies can be collected.
	s.State.Pending = append([]*wechat.AddMsg(nil), previousPending[count:]...)
	if err := s.Save(); err != nil {
		s.State.PendingBase, s.State.Pending = previousBase, previousPending
		return 0, err
	}
	return count, nil
}
