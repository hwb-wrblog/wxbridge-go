package workflow

import (
	"encoding/xml"
	"errors"
	"testing"
	"time"

	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func emojiTestAck(req *wechat.SendAppMsgRequest) *wechat.SendAppMsgResponse {
	return &wechat.SendAppMsgResponse{
		BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(0)},
		FromUserName: req.Msg.FromUserName, ToUserName: req.Msg.ToUserName,
		ClientMsgId: req.Msg.ClientMsgId, Type: proto.Uint32(8),
		MsgId: proto.Uint32(17), NewMsgId: proto.Uint64(1<<63 + 19),
	}
}

func TestMediaEmojiUsesTargetAppMessagePath(t *testing.T) {
	const digest = "0123456789ABCDEF0123456789ABCDEF"
	const normalized = "0123456789abcdef0123456789abcdef"
	base := &wechat.BaseRequest{Uin: proto.Uint32(7)}
	now := time.Unix(1700000000, 0)
	calls := 0
	exchange := func(path string, cgi uint32, request, response proto.Message) error {
		calls++
		if path != "/cgi-bin/micromsg-bin/sendappmsg" || cgi != 222 {
			t.Fatalf("emoji used the wrong target path: %s %d", path, cgi)
		}
		req := request.(*wechat.SendAppMsgRequest)
		if req.BaseRequest != base || req.GetMsgForwardType() != 2 || req.DirectShare != nil || req.FromSence != nil || req.SendMsgTicket == nil || req.GetSendMsgTicket() != "" {
			t.Fatal("target SendAppMsgRequest fields changed")
		}
		msg := req.GetMsg()
		if msg.GetFromUserName() != "sender" || msg.GetToUserName() != "receiver" || msg.GetClientMsgId() != "emoji-client" || msg.GetType() != 8 || msg.GetCreateTime() != uint32(now.Unix()) {
			t.Fatal("emoji message identity or type changed")
		}
		if msg.SdkVersion == nil || msg.GetSdkVersion() != 0 || msg.Thumb == nil || msg.GetThumb().GetILen() != 0 || len(msg.GetThumb().GetBuffer()) != 0 {
			t.Fatal("emoji zero-valued fields missing")
		}
		var content struct {
			App struct {
				Type       int `xml:"type"`
				Attachment struct {
					Size      int32  `xml:"totallen"`
					ID        string `xml:"attachid"`
					MD5       string `xml:"emoticonmd5"`
					Extension string `xml:"fileext"`
				} `xml:"appattach"`
			} `xml:"appmsg"`
		}
		if err := xml.Unmarshal([]byte("<msg>"+msg.GetContent()+"</msg>"), &content); err != nil {
			t.Fatal(err)
		}
		if content.App.Type != 8 || content.App.Attachment.Size != 1234 || content.App.Attachment.ID != "0:0:"+normalized || content.App.Attachment.MD5 != normalized || content.App.Attachment.Extension != "pic" {
			t.Fatalf("incorrect emoji XML: %+v", content)
		}
		*response.(*wechat.SendAppMsgResponse) = *emojiTestAck(req)
		return nil
	}
	receipt, err := sendEmojiReference(base, "sender", "receiver", digest, 1234, "emoji-client", now, exchange)
	if err != nil || calls != 1 || receipt == nil || !receipt.ServerAccepted || receipt.MessageID != "9223372036854775827" || receipt.Bytes != 1234 || receipt.Kind != "emoji" {
		t.Fatalf("emoji failed: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
	}
}

func TestMediaEmojiValidation(t *testing.T) {
	for _, digest := range []string{"", "short", "0123456789abcdef0123456789abcdef ", "g123456789abcdef0123456789abcdef", "<script>0123456789abcdef</script>"} {
		if _, err := validateEmojiReference(digest, 1); err == nil {
			t.Fatalf("invalid emoji MD5 accepted: %q", digest)
		}
	}
	for _, size := range []int32{0, -1, -2147483648} {
		if _, err := validateEmojiReference("0123456789abcdef0123456789abcdef", size); err == nil {
			t.Fatal("invalid emoji size accepted")
		}
	}
	if _, err := (&Session{}).SendEmoji("receiver", "0123456789abcdef0123456789abcdef", 123); err == nil {
		t.Fatal("emoji sent without authentication")
	}
}

func TestMediaEmojiRejectsUnconfirmedResponsesWithoutRetry(t *testing.T) {
	for name, mutate := range map[string]func(*wechat.SendAppMsgResponse){
		"no response":     func(r *wechat.SendAppMsgResponse) { r.BaseResponse = nil },
		"no result":       func(r *wechat.SendAppMsgResponse) { r.BaseResponse.Ret = nil },
		"rejected":        func(r *wechat.SendAppMsgResponse) { r.BaseResponse.Ret = proto.Int32(-1) },
		"wrong recipient": func(r *wechat.SendAppMsgResponse) { r.ToUserName = proto.String("other") },
		"wrong sender":    func(r *wechat.SendAppMsgResponse) { r.FromUserName = proto.String("other") },
		"no client ID":    func(r *wechat.SendAppMsgResponse) { r.ClientMsgId = nil },
		"wrong client ID": func(r *wechat.SendAppMsgResponse) { r.ClientMsgId = proto.String("other") },
		"wrong type":      func(r *wechat.SendAppMsgResponse) { r.Type = proto.Uint32(6) },
		"no message ID":   func(r *wechat.SendAppMsgResponse) { r.MsgId, r.NewMsgId = nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			exchange := func(_ string, _ uint32, request, response proto.Message) error {
				calls++
				ack := emojiTestAck(request.(*wechat.SendAppMsgRequest))
				mutate(ack)
				*response.(*wechat.SendAppMsgResponse) = *ack
				return nil
			}
			receipt, err := sendEmojiReference(&wechat.BaseRequest{}, "sender", "receiver", "0123456789abcdef0123456789abcdef", 123, "client", time.Now(), exchange)
			if err == nil || calls != 1 || receipt == nil || receipt.ServerAccepted {
				t.Fatalf("invalid emoji response accepted or retried: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
			}
		})
	}
	calls := 0
	failure := errors.New("offline transport failure")
	exchange := func(string, uint32, proto.Message, proto.Message) error {
		calls++
		return failure
	}
	receipt, err := sendEmojiReference(&wechat.BaseRequest{}, "sender", "receiver", "0123456789abcdef0123456789abcdef", 123, "client", time.Now(), exchange)
	if !errors.Is(err, failure) || calls != 1 || receipt.ServerAccepted {
		t.Fatalf("emoji transport failure was retried: %+v %v", receipt, err)
	}
}
