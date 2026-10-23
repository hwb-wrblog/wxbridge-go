package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func TestMessageConversationAndSender(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, content, conversation, sender, direction, text string
		group                                                          bool
	}{
		{"private incoming", "friend", "wxid_self", "hello", "friend", "friend", "incoming", "hello", false},
		{"private outgoing", "wxid_self", "friend", "hello", "friend", "wxid_self", "outgoing", "hello", false},
		{"file helper", "wxid_self", "filehelper", "note", "filehelper", "wxid_self", "outgoing", "note", false},
		{"group incoming", "123@chatroom", "wxid_self", "wxid_member:\nhello", "123@chatroom", "wxid_member", "incoming", "hello", true},
		{"group outgoing", "wxid_self", "123@chatroom", "literal:\nhello", "123@chatroom", "wxid_self", "outgoing", "literal:\nhello", true},
		{"group own sync", "123@chatroom", "wxid_self", "wxid_self:\nhello", "123@chatroom", "wxid_self", "outgoing", "hello", true},
		{"group no sender", "123@chatroom", "wxid_self", "hello", "123@chatroom", "", "incoming", "hello", true},
		{"group malformed prefix", "123@chatroom", "wxid_self", "<msg>:\nhello", "123@chatroom", "", "incoming", "<msg>:\nhello", true},
		{"private colon preserved", "friend", "wxid_self", "wxid_member:\nhello", "friend", "friend", "incoming", "wxid_member:\nhello", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := &wechat.AddMsg{
				NewMsgId: proto.Int64(9007199254740993), MsgType: proto.Uint32(1), CreateTime: proto.Uint32(123),
				FromUserName: &wechat.SKBuiltinString{Str: proto.String(tc.from)},
				ToUserName:   &wechat.SKBuiltinString{Str: proto.String(tc.to)}, Content: &wechat.SKBuiltinString{Str: proto.String(tc.content)},
			}
			got := projectMessage(message, "wxid_self")
			for key, want := range map[string]interface{}{
				"id": "9007199254740993", "from": tc.from, "to": tc.to, "type": uint32(1), "created": uint32(123),
				"conversation_id": tc.conversation, "sender_id": tc.sender, "direction": tc.direction, "text": tc.text, "is_group": tc.group, "mentions_me": false,
			} {
				if got[key] != want {
					t.Errorf("%s = %#v, want %#v", key, got[key], want)
				}
			}
		})
	}
}

func TestMessageMentionsAreStructuredAndExact(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         bool
	}{
		{"exact", "<msgsource><atuserlist>wxid_self</atuserlist></msgsource>", true},
		{"list", "<msgsource><atuserlist><![CDATA[wxid_other, wxid_self]]></atuserlist></msgsource>", true},
		{"all", "<msgsource><atuserlist>notify@all</atuserlist></msgsource>", true},
		{"substring", "<msgsource><atuserlist>wxid_self_other</atuserlist></msgsource>", false},
		{"different field", "<msgsource><text>wxid_self</text></msgsource>", false},
		{"wrong root", "<other><atuserlist>wxid_self</atuserlist></other>", false},
		{"malformed", "<msgsource><atuserlist>wxid_self</msgsource>", false},
		{"second root", "<msgsource><atuserlist>wxid_self</atuserlist></msgsource><other/>", false},
		{"trailing garbage", "<msgsource><atuserlist>wxid_self</atuserlist></msgsource>junk", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := &wechat.AddMsg{FromUserName: &wechat.SKBuiltinString{Str: proto.String("123@chatroom")},
				Content: &wechat.SKBuiltinString{Str: proto.String("wxid_member:\nhello")}, MsgType: proto.Uint32(1), MsgSource: proto.String(tc.source)}
			if got := projectMessage(message, "wxid_self")["mentions_me"]; got != tc.want {
				t.Fatalf("mentions_me = %v, want %v", got, tc.want)
			}
			if got := projectMessage(message, "")["mentions_me"]; got != false {
				t.Fatal("unknown self identity reported as mentioned")
			}
		})
	}
}

func TestMessageProjectionHidesMediaContent(t *testing.T) {
	message := &wechat.AddMsg{FromUserName: &wechat.SKBuiltinString{Str: proto.String("123@chatroom")},
		Content: &wechat.SKBuiltinString{Str: proto.String("wxid_member:\n<msg aeskey=\"private-key\"/>")}, MsgType: proto.Uint32(3),
		MsgSource: proto.String("<msgsource><secret>private-source</secret></msgsource>")}
	got := projectMessage(message, "wxid_self")
	data, err := json.Marshal(got)
	if err != nil || got["text"] != "" || got["sender_id"] != "wxid_member" || strings.Contains(string(data), "private-") {
		t.Fatalf("invalid public projection: %s (%v)", data, err)
	}
	if got := projectMessage(nil, ""); got["direction"] != "incoming" || got["text"] != "" {
		t.Fatal("nil message projection is not safe")
	}
}
