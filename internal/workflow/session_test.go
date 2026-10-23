package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"feiyu.com/wx/clientsdk/baseinfo"
	"feiyu.com/wx/clientsdk/mmtls"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func TestMessageAcceptanceRequiresServerAcknowledgement(t *testing.T) {
	r := &wechat.NewSendMsgResponse{BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(0)}}
	if ValidateSend(r, "filehelper") == nil {
		t.Fatal("empty success envelope accepted")
	}
	r.Count = proto.Uint32(1)
	ack := &wechat.MicroMsgResponseNew{Ret: proto.Uint32(0), ToUserName: &wechat.SKBuiltinString{Str: proto.String("filehelper")}, NewMsgId: proto.Uint64(123)}
	r.ChatSendRetList = []*wechat.MicroMsgResponseNew{ack}
	if err := ValidateSend(r, "filehelper"); err != nil {
		t.Fatal(err)
	}
	ack.Ret = proto.Uint32(4)
	if ValidateSend(r, "filehelper") == nil {
		t.Fatal("per-message failure accepted")
	}
	ack.Ret = nil
	if ValidateSend(r, "filehelper") == nil {
		t.Fatal("missing ret accepted")
	}
	ack.Ret = proto.Uint32(0)
	if ValidateSend(r, "different") == nil {
		t.Fatal("wrong recipient accepted")
	}
	ack.NewMsgId = nil
	if ValidateSend(r, "filehelper") == nil {
		t.Fatal("missing message ID accepted")
	}
}

func TestSyncDecodesMessagesAndRejectsTruncation(t *testing.T) {
	msg := &wechat.AddMsg{FromUserName: &wechat.SKBuiltinString{Str: proto.String("sender")}, MsgType: proto.Uint32(1), Content: &wechat.SKBuiltinString{Str: proto.String("reply")}}
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	item := &wechat.CmdItem{CmdId: proto.Uint32(5), CmdBuf: &wechat.DATA{Len: proto.Uint32(uint32(len(data))), Data: data}}
	r := &wechat.NewSyncResponse{Ret: proto.Int32(0), CmdList: &wechat.CmdList{Count: proto.Uint32(1), ItemList: []*wechat.CmdItem{item}}}
	msgs, err := DecodeSync(r)
	if err != nil || len(msgs) != 1 || msgs[0].GetContent().GetStr() != "reply" {
		t.Fatalf("decode: %v, %v", msgs, err)
	}
	r.CmdList.Count = proto.Uint32(2)
	if _, err := DecodeSync(r); err == nil {
		t.Fatal("truncated batch accepted")
	}
	r.CmdList.Count = proto.Uint32(1)
	item.CmdBuf.Data = []byte{0xff}
	if _, err := DecodeSync(r); err == nil {
		t.Fatal("corrupt message accepted")
	}
	r.Ret = nil
	if _, err := DecodeSync(r); err == nil {
		t.Fatal("missing sync result accepted")
	}
}

func TestSessionPersistsCursorAndMessagesPrivately(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.State.User = &baseinfo.UserInfo{DeviceInfo: &baseinfo.DeviceInfo{}, SyncKey: []byte{1, 2}, MMInfo: &mmtls.MMInfo{PskAccessKey: []byte("transport-only")}}
	s.State.Pending = []*wechat.AddMsg{{MsgId: proto.Uint32(123)}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "session.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v %v", info, err)
	}
	restored, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if restored.State.User.MMInfo != nil || len(restored.State.User.SyncKey) != 2 || len(restored.State.Pending) != 1 {
		t.Fatal("state lost or transport persisted")
	}
}

func TestNoLoginOrMessagesWithoutPhoneConfirmation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Login(); err == nil {
		t.Fatal("login accepted without scan")
	}
	if _, err := s.Send("filehelper", "test"); err == nil {
		t.Fatal("send accepted without auth")
	}
	if _, _, err := s.Sync(); err == nil {
		t.Fatal("sync accepted without auth")
	}
}
