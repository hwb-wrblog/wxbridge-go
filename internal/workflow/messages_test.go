package workflow

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"feiyu.com/wx/clientsdk/baseinfo"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func TestAckMessagesPersistsAndKeepsCursorsStable(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.State.User = &baseinfo.UserInfo{DeviceInfo: &baseinfo.DeviceInfo{}, SyncKey: []byte("sync-cursor")}
	s.State.Pending = []*wechat.AddMsg{
		{MsgId: proto.Uint32(1), Content: &wechat.SKBuiltinString{Str: proto.String("processed-body")}},
		{MsgId: proto.Uint32(2)},
		{MsgId: proto.Uint32(3)},
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	// Old session files have no base and keep their original cursor positions.
	s, err = Open(s.Dir)
	if err != nil || s.State.PendingBase != 0 || s.MessageCursor() != 3 {
		t.Fatal("legacy cursor migration", err)
	}
	if count, err := s.AckMessages(2); err != nil || count != 2 || s.MessageCursor() != 3 {
		t.Fatal("ack changed end cursor", count, err)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "session.json"))
	if err != nil || bytes.Contains(data, []byte("processed-body")) {
		t.Fatal("acknowledged body remains on disk", err)
	}
	s, err = Open(s.Dir)
	if err != nil || s.State.PendingBase != 2 || len(s.State.Pending) != 1 || s.State.Pending[0].GetMsgId() != 3 || string(s.State.User.SyncKey) != "sync-cursor" {
		t.Fatal("restart changed inbox or upstream cursor", err)
	}
	for _, cursor := range []uint64{0, 1, 2} {
		if count, err := s.AckMessages(cursor); err != nil || count != 0 || s.State.PendingBase != 2 {
			t.Fatal("ack replay is not idempotent", cursor, count, err)
		}
	}
	s.State.Pending = append(s.State.Pending, &wechat.AddMsg{MsgId: proto.Uint32(4)})
	if s.MessageCursor() != 4 {
		t.Fatal("new message reused an old cursor")
	}
	if _, err := s.AckMessages(5); err == nil || len(s.State.Pending) != 2 {
		t.Fatal("future cursor accepted")
	}
	if count, err := s.AckMessages(4); err != nil || count != 2 || len(s.State.Pending) != 0 || s.State.PendingBase != 4 {
		t.Fatal("final ack failed", count, err)
	}
}

func TestAckMessagesSaveFailureRetainsInbox(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.State.PendingBase = 10
	s.State.Pending = []*wechat.AddMsg{{MsgId: proto.Uint32(11)}, {MsgId: proto.Uint32(12)}}
	if err := os.Mkdir(filepath.Join(s.Dir, "session.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if count, err := s.AckMessages(11); err == nil || count != 0 {
		t.Fatal("failed save reported success", count, err)
	}
	if s.State.PendingBase != 10 || len(s.State.Pending) != 2 || s.State.Pending[0].GetMsgId() != 11 {
		t.Fatal("failed save consumed messages")
	}
}
