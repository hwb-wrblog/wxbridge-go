package workflow

import (
	"fmt"
	"os"
	"path/filepath"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/protobuf/wechat"
)

// Logout revokes the remote session before discarding local account data.
func (s *Session) Logout() error {
	return s.logout(func() (*wechat.LogOutResponse, error) {
		if err := s.connect(); err != nil {
			return nil, err
		}
		header, err := clientsdk.SendLogOutRequest(s.State.User)
		if err != nil {
			return nil, err
		}
		response := &wechat.LogOutResponse{}
		if err := clientsdk.ParseResponseData(s.State.User, header, response); err != nil {
			return nil, err
		}
		return response, nil
	})
}

func (s *Session) logout(remote func() (*wechat.LogOutResponse, error)) error {
	if s.State.LogoutPending {
		return s.finishLogout()
	}
	if s.State.Authenticated {
		response, err := remote()
		if err != nil {
			return fmt.Errorf("remote logout: %w", err)
		}
		if err := responseOK(response.GetBaseResponse()); err != nil {
			return fmt.Errorf("remote logout: %w", err)
		}
	}
	s.State = State{
		ClientVersion: ClientVersion,
		PendingBase:   s.State.PendingBase + uint64(len(s.State.Pending)),
		LogoutPending: true,
	}
	return s.finishLogout()
}

// Keep a durable cleanup marker until all account files are removed. Open and
// QR resume this work, so an interrupted logout cannot mix two accounts' data.
func (s *Session) finishLogout() error {
	if !s.State.LogoutPending {
		return nil
	}
	if err := s.Save(); err != nil {
		return fmt.Errorf("persist logout cleanup: %w", err)
	}
	for _, name := range []string{"media", "uploads", "submissions"} {
		if err := os.RemoveAll(filepath.Join(s.Dir, name)); err != nil {
			return fmt.Errorf("logout cleanup %s: %w", name, err)
		}
	}
	for _, name := range []string{"media-index.json", "api-state.json", "qrcode.jpeg", "qrcode.jpg", "qrcode.png", "qrcode.base64", "qrcode.base64.txt"} {
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logout cleanup %s: %w", name, err)
		}
	}
	for _, name := range []string{"uploads", "submissions"} {
		if err := os.MkdirAll(filepath.Join(s.Dir, name), 0700); err != nil {
			return fmt.Errorf("recreate %s: %w", name, err)
		}
	}
	s.State.LogoutPending = false
	if err := s.Save(); err != nil {
		s.State.LogoutPending = true
		return fmt.Errorf("complete logout cleanup: %w", err)
	}
	return nil
}
