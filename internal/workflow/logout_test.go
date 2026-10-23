package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"feiyu.com/wx/clientsdk/baseinfo"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func logoutTestSession(t *testing.T) *Session {
	t.Helper()
	s := &Session{Dir: t.TempDir(), State: State{
		ClientVersion: ClientVersion,
		Authenticated: true,
		User: &baseinfo.UserInfo{
			WxId: "previous-account", NickName: "Previous", HeadURL: "https://example.invalid/avatar",
			DeviceInfo: &baseinfo.DeviceInfo{}, SessionKey: []byte("private-session-key"),
		},
		QRKey: []byte("private-qr-key"), PendingBase: 20,
		Pending: []*wechat.AddMsg{{MsgId: proto.Uint32(1)}, {MsgId: proto.Uint32(2)}},
	}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s
}

func successfulLogout() (*wechat.LogOutResponse, error) {
	return &wechat.LogOutResponse{BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(0)}}, nil
}

func TestLogoutClearsAccountAndPreservesInstanceCredentials(t *testing.T) {
	s := logoutTestSession(t)
	for _, name := range []string{"media", "uploads", "submissions"} {
		dir := filepath.Join(s.Dir, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "old-account"), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"api-token", ".lock", "media-index.json", "api-state.json", "qrcode.jpeg", "qrcode.png", "qrcode.base64.txt"} {
		if err := os.WriteFile(filepath.Join(s.Dir, name), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := os.Stat(filepath.Join(s.Dir, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.logout(successfulLogout); err != nil {
		t.Fatal(err)
	}
	if s.State.Authenticated || s.State.User != nil || s.State.LogoutPending || len(s.State.Pending) != 0 || len(s.State.QRKey) != 0 || s.State.PendingBase != 22 {
		t.Fatal("account state was not cleared")
	}
	for _, name := range []string{"media", "media-index.json", "api-state.json", "qrcode.jpeg", "qrcode.png", "qrcode.base64.txt"} {
		if _, err := os.Stat(filepath.Join(s.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("account artifact %s remains: %v", name, err)
		}
	}
	for _, name := range []string{"uploads", "submissions"} {
		dir := filepath.Join(s.Dir, name)
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s was not recreated empty: %v", name, err)
		}
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("%s is not private: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "api-token"))
	if err != nil || string(data) != "private" {
		t.Fatal("instance token changed", err)
	}
	newLock, err := os.Stat(filepath.Join(s.Dir, ".lock"))
	if err != nil || !os.SameFile(lock, newLock) {
		t.Fatal("state lock changed", err)
	}
	data, err = os.ReadFile(filepath.Join(s.Dir, "session.json"))
	if err != nil || strings.Contains(string(data), "private-") || strings.Contains(string(data), "previous-account") {
		t.Fatal("persisted credentials remain", err)
	}
	reopened, err := Open(s.Dir)
	if err != nil || reopened.State.User != nil || reopened.State.PendingBase != 22 {
		t.Fatal("logged-out state cannot reopen", err)
	}
	if err := reopened.Logout(); err != nil || reopened.State.PendingBase != 22 {
		t.Fatal("logout is not idempotent", err)
	}
}

func TestLogoutFailureRetainsAuthenticatedState(t *testing.T) {
	for _, tt := range []struct {
		name     string
		response *wechat.LogOutResponse
		err      error
	}{
		{name: "transport", err: errors.New("network unavailable")},
		{name: "rejected", response: &wechat.LogOutResponse{BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(-1)}}},
		{name: "empty response"},
		{name: "missing result", response: &wechat.LogOutResponse{BaseResponse: &wechat.BaseResponse{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := logoutTestSession(t)
			before, err := os.ReadFile(filepath.Join(s.Dir, "session.json"))
			if err != nil {
				t.Fatal(err)
			}
			err = s.logout(func() (*wechat.LogOutResponse, error) { return tt.response, tt.err })
			if err == nil || !s.State.Authenticated || s.State.User.WxId != "previous-account" || s.State.LogoutPending || len(s.State.Pending) != 2 || s.State.PendingBase != 20 {
				t.Fatal("failed logout discarded state", err)
			}
			after, err := os.ReadFile(filepath.Join(s.Dir, "session.json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("failed logout changed persisted state", err)
			}
		})
	}
}

func TestLogoutClearsUnconfirmedQRWithoutRemoteRequest(t *testing.T) {
	s := logoutTestSession(t)
	s.State.Authenticated = false
	if err := s.logout(func() (*wechat.LogOutResponse, error) {
		t.Fatal("unauthenticated logout made remote request")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if s.State.User != nil || s.State.Notify != nil || s.State.QRKey != nil {
		t.Fatal("QR identity retained")
	}
}

func TestLogoutCleanupFailureIsResumedOnOpen(t *testing.T) {
	s := logoutTestSession(t)
	obstruction := filepath.Join(s.Dir, "media-index.json")
	if err := os.MkdirAll(obstruction, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(obstruction, "obstruction"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.logout(successfulLogout); err == nil || !s.State.LogoutPending || s.State.Authenticated || s.State.User != nil {
		t.Fatal("cleanup failure was not retained", err)
	}
	if _, err := Open(s.Dir); err == nil {
		t.Fatal("incomplete cleanup allowed reopen")
	}
	if err := os.RemoveAll(obstruction); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State.LogoutPending || reopened.State.User != nil || reopened.State.PendingBase != 22 {
		t.Fatal("cleanup was not resumed")
	}
}

func TestLogoutPersistenceFailureBlocksFreshQRUntilCleanup(t *testing.T) {
	s := logoutTestSession(t)
	path := filepath.Join(s.Dir, "session.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.logout(successfulLogout); err == nil || !s.State.LogoutPending || s.State.Authenticated {
		t.Fatal("persistence failure did not block new session", err)
	}
	if _, err := s.QR(); err == nil || !s.State.LogoutPending || s.State.User != nil {
		t.Fatal("QR bypassed unfinished logout", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.logout(func() (*wechat.LogOutResponse, error) {
		t.Fatal("retry repeated remote logout")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if s.State.LogoutPending || s.State.PendingBase != 22 {
		t.Fatal("logout retry failed")
	}
}
