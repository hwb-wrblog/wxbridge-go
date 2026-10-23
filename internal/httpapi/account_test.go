package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"feiyu.com/wx/clientsdk/baseinfo"
	"feiyu.com/wx/internal/workflow"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func accountPayload(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid response %q: %v", w.Body.String(), err)
	}
	return body
}

func inboxMessage(id int64, kind uint32, from string) *wechat.AddMsg {
	return &wechat.AddMsg{NewMsgId: proto.Int64(id), MsgType: proto.Uint32(kind),
		FromUserName: &wechat.SKBuiltinString{Str: proto.String(from)},
		ToUserName:   &wechat.SKBuiltinString{Str: proto.String("wxid_self")},
		Content:      &wechat.SKBuiltinString{Str: proto.String("test message")}}
}

func TestStatusIdentityAndLoggedOutPrivacy(t *testing.T) {
	s := testServer(t)
	s.session.State.User = &baseinfo.UserInfo{WxId: "wxid_self", NickName: "Test account", HeadURL: "https://example.com/avatar.png", SessionKey: []byte("private-session-key")}
	s.session.State.Authenticated = true
	w := requestAPI(s, "GET", "/v1/status", "", true)
	body := accountPayload(t, w)
	for key, want := range map[string]interface{}{"authenticated": true, "wxid": "wxid_self", "nickname": "Test account", "avatar_url": "https://example.com/avatar.png"} {
		if w.Code != 200 || body[key] != want {
			t.Fatalf("%s = %#v, want %#v: %s", key, body[key], want, w.Body.String())
		}
	}
	if strings.Contains(w.Body.String(), "private-session-key") || strings.Contains(w.Body.String(), "aeskey") {
		t.Fatal("status exposed session material")
	}
	for _, user := range []*baseinfo.UserInfo{s.session.State.User, nil} {
		s.session.State.Authenticated, s.session.State.User = false, user
		w = requestAPI(s, "GET", "/v1/status", "", true)
		body = accountPayload(t, w)
		if w.Code != 200 || body["authenticated"] != false || body["wxid"] != "" || body["nickname"] != "" || body["avatar_url"] != "" {
			t.Fatalf("logged out identity is not empty: %s", w.Body.String())
		}
	}
}

func TestLogoutHTTPUnconfirmedSessionIsClearedAndRepeatable(t *testing.T) {
	s := testServer(t)
	s.session.State.User = &baseinfo.UserInfo{WxId: "unconfirmed-account", ShortHost: "invalid.invalid"}
	s.session.State.QRKey = []byte("old-qr-key")
	s.session.State.PendingBase = 10
	s.session.State.Pending = []*wechat.AddMsg{inboxMessage(1, 1, "friend")}
	s.lastSync, s.lastHeartbeat, s.lastError = 11, 12, "old error"
	for _, name := range []string{"uploads/old", "submissions/old", "media/old", "media-index.json", "qrcode.png"} {
		path := filepath.Join(s.session.Dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("old account data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	token, err := TokenFile(s.session.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestAPI(s, "POST", "/v1/logout", "", false); got.Code != 401 || s.session.State.User == nil {
		t.Fatal("unauthenticated logout was not rejected")
	}
	for i := 0; i < 2; i++ {
		w := requestAPI(s, "POST", "/v1/logout", "", true)
		body := accountPayload(t, w)
		identity, ok := body["result"].(map[string]interface{})
		if w.Code != 200 || !ok || identity["authenticated"] != false || identity["wxid"] != "" {
			t.Fatalf("logout failed: %s", w.Body.String())
		}
		if s.session.State.User != nil || len(s.session.State.Pending) != 0 || s.session.State.PendingBase != 11 || s.lastSync != 0 || s.lastHeartbeat != 0 || s.lastError != "" {
			t.Fatal("logout did not clear account state or preserve cursor")
		}
	}
	for _, name := range []string{"uploads/old", "submissions/old", "media/old", "media-index.json", "qrcode.png"} {
		if _, err := os.Stat(filepath.Join(s.session.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("old account file %s retained: %v", name, err)
		}
	}
	if saved, err := TokenFile(s.session.Dir); err != nil || saved != token {
		t.Fatal("logout changed instance API credentials", err)
	}
	if _, err := workflow.Open(s.session.Dir); err != nil {
		t.Fatalf("logged out state cannot be reopened: %v", err)
	}
	if got := requestAPI(s, "POST", "/v1/uploads", "new upload", true); got.Code != 201 {
		t.Fatal("upload directory was not recreated", got.Body.String())
	}
}

func TestMessageCursorBoundsAndCumulativeAckSurviveRestart(t *testing.T) {
	s := testServer(t)
	s.session.State.ClientVersion = workflow.ClientVersion
	s.session.State.PendingBase = 10
	s.session.State.Pending = []*wechat.AddMsg{inboxMessage(1, 1, "friend"), inboxMessage(2, 1, "friend"), inboxMessage(3, 1, "friend")}
	for _, tc := range []struct {
		query string
		code  int
	}{
		{"?cursor=9", 410}, {"?cursor=10", 200}, {"?cursor=13", 200}, {"?cursor=14", 400}, {"?cursor=-1", 400}, {"?cursor=bad", 400},
	} {
		w := requestAPI(s, "GET", "/v1/messages"+tc.query, "", true)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.query, w.Code, w.Body.String())
		}
		if tc.code == 410 && accountPayload(t, w)["acked_cursor"] != float64(10) {
			t.Fatal("stale cursor response lacks recovery cursor")
		}
	}
	w := requestAPI(s, "GET", "/v1/messages?limit=2", "", true)
	body := accountPayload(t, w)
	if w.Code != 200 || body["next_cursor"] != float64(12) || body["more"] != true || len(body["messages"].([]interface{})) != 2 {
		t.Fatalf("unexpected page: %s", w.Body.String())
	}
	// This endpoint must be routed before the generic send handler: no send key is supplied.
	w = requestAPI(s, "POST", "/v1/messages/ack", `{"cursor":12}`, true)
	ack := accountPayload(t, w)["result"].(map[string]interface{})
	if w.Code != 200 || ack["acknowledged"] != float64(2) || ack["pending_messages"] != float64(1) || s.session.State.Pending[0].GetNewMsgId() != 3 {
		t.Fatalf("ack was not cumulative and exclusive: %s", w.Body.String())
	}
	reopened, err := workflow.Open(s.session.Dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err = New(reopened, Config{Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []string{"12", "10"} {
		w = requestAPI(s, "POST", "/v1/messages/ack", `{"cursor":`+cursor+`}`, true)
		ack = accountPayload(t, w)["result"].(map[string]interface{})
		if w.Code != 200 || ack["acknowledged"] != float64(0) || ack["acked_cursor"] != float64(12) || len(s.session.State.Pending) != 1 {
			t.Fatalf("replayed ack changed inbox: %s", w.Body.String())
		}
	}
	w = requestAPI(s, "GET", "/v1/messages", "", true)
	body = accountPayload(t, w)
	if w.Code != 200 || body["next_cursor"] != float64(13) || body["acked_cursor"] != float64(12) || body["more"] != false || len(body["messages"].([]interface{})) != 1 {
		t.Fatalf("remaining page after restart: %s", w.Body.String())
	}
	if got := requestAPI(s, "GET", "/v1/messages?cursor=10", "", true); got.Code != 410 {
		t.Fatal("old cursor reused after acknowledgement")
	}
}

func TestMessageAckRejectsInvalidPayloadWithoutMutation(t *testing.T) {
	s := testServer(t)
	s.session.State.Pending = []*wechat.AddMsg{inboxMessage(1, 1, "friend")}
	for _, body := range []string{"", "null", "{}", `{"cursor":null}`, `{"cursor":-1}`, `{"cursor":0.5}`, `{"cursor":"1"}`, `{"cursor":2}`, `{"cursor":1,"unknown":true}`, `{"cursor":1} {}`} {
		w := requestAPI(s, "POST", "/v1/messages/ack", body, true)
		if w.Code != 400 || len(s.session.State.Pending) != 1 || s.session.State.PendingBase != 0 {
			t.Fatalf("invalid ack %q changed inbox: %d %s", body, w.Code, w.Body.String())
		}
	}
}

func TestMessageAckProtectsIncomingMediaUnlessExplicitlyDiscarded(t *testing.T) {
	for _, status := range []string{"missing", "failed", "ready"} {
		t.Run(status, func(t *testing.T) {
			s := testServer(t)
			s.session.State.Pending = []*wechat.AddMsg{inboxMessage(1, 1, "friend"), inboxMessage(2, 3, "friend")}
			if status == "ready" {
				if err := os.MkdirAll(filepath.Join(s.session.Dir, "media"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(s.session.Dir, "media", "2.png"), []byte("image"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if status != "missing" {
				data, _ := json.Marshal(map[string]*workflow.MediaFile{"2": {MessageID: "2", Status: status, Kind: "image", Filename: "2.png", Bytes: 5}})
				if err := os.WriteFile(filepath.Join(s.session.Dir, "media-index.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			w := requestAPI(s, "POST", "/v1/messages/ack", `{"cursor":2}`, true)
			if status == "ready" {
				if w.Code != 200 || len(s.session.State.Pending) != 0 {
					t.Fatalf("downloaded media blocked acknowledgement: %s", w.Body.String())
				}
				return
			}
			if w.Code != 409 || len(s.session.State.Pending) != 2 || s.session.State.PendingBase != 0 || accountPayload(t, w)["message_id"] != "2" {
				t.Fatalf("pending media lost during acknowledgement: %s", w.Body.String())
			}
			w = requestAPI(s, "POST", "/v1/messages/ack", `{"cursor":2,"discard_pending_media":true}`, true)
			if w.Code != 200 || len(s.session.State.Pending) != 0 || s.session.State.PendingBase != 2 {
				t.Fatalf("explicit discard did not acknowledge media: %s", w.Body.String())
			}
		})
	}
}

func TestLogoutPendingBlocksAccountOperations(t *testing.T) {
	s := testServer(t)
	s.session.State.LogoutPending = true
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/media/123", ""},
		{"POST", "/v1/uploads", "test bytes"},
		{"POST", "/v1/messages/text", `{"to":"friend","text":"hello"}`},
		{"POST", "/v1/messages/ack", `{"cursor":0}`},
		{"GET", "/v1/messages", ""},
		{"POST", "/v1/login/qr", ""},
	} {
		w := requestAPI(s, tc.method, tc.path, tc.body, true)
		if w.Code != 409 {
			t.Fatalf("%s %s bypassed pending cleanup: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	w := requestAPI(s, "GET", "/v1/status", "", true)
	if w.Code != 200 || accountPayload(t, w)["logout_pending"] != true {
		t.Fatal("status unavailable during cleanup", w.Body.String())
	}
	if w := requestAPI(s, "GET", "/healthz", "", true); w.Code != 200 {
		t.Fatal("health unavailable during cleanup")
	}
	w = requestAPI(s, "POST", "/v1/logout", "", true)
	if w.Code != 200 || s.session.State.LogoutPending {
		t.Fatal("cleanup cannot be resumed through logout", w.Body.String())
	}
}

func TestMessageAckDoesNotRequireDownloadingOwnGroupMedia(t *testing.T) {
	s := testServer(t)
	s.session.State.Authenticated = true
	s.session.State.User = &baseinfo.UserInfo{WxId: "wxid_self"}
	m := inboxMessage(1, 3, "room@chatroom")
	m.Content.Str = proto.String("wxid_self:\n<msg><img/></msg>")
	s.session.State.Pending = []*wechat.AddMsg{m}
	w := requestAPI(s, "POST", "/v1/messages/ack", `{"cursor":1}`, true)
	if w.Code != 200 || len(s.session.State.Pending) != 0 {
		t.Fatalf("own group media blocked acknowledgement: %s", w.Body.String())
	}
}
