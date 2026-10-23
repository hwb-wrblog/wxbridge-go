package workflow

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/clientsdk/mmtls"
	"github.com/golang/protobuf/proto"
)

// MediaReceipt reports a server acknowledgement, not recipient playback.
type MediaReceipt struct {
	Kind           string `json:"kind"`
	Recipient      string `json:"recipient"`
	ClientID       string `json:"client_id"`
	MessageID      string `json:"message_id"`
	Bytes          int    `json:"bytes"`
	Chunks         int    `json:"chunks"`
	ServerAccepted bool   `json:"server_accepted"`
	UploadComplete bool   `json:"upload_complete,omitempty"`
}

type mediaExchange func(path string, cgi uint32, request, response proto.Message) error

func (s *Session) prepareMedia(to string) error {
	if !s.State.Authenticated || s.State.User == nil {
		return errors.New("not authenticated")
	}
	if strings.TrimSpace(to) == "" || strings.TrimSpace(to) != to {
		return errors.New("recipient is required and must not have surrounding whitespace")
	}
	return s.connect()
}

func (s *Session) exchangeMedia(path string, cgi uint32, request, response proto.Message) error {
	data, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	u := s.State.User
	raw, err := mmtls.MMHTTPPostData(u.MMInfo, path, clientsdk.Pack(u, data, cgi, 5))
	if err != nil {
		return fmt.Errorf("media transport failed; delivery may be unknown, do not retry blindly: %w", err)
	}
	header, err := clientsdk.DecodePackHeader(raw, nil)
	if err != nil {
		return err
	}
	return clientsdk.ParseResponseData(u, header, response)
}

func (s *Session) finishMedia(receipt *MediaReceipt) (*MediaReceipt, error) {
	if err := s.Save(); err != nil {
		return receipt, fmt.Errorf("media acknowledged but session save failed; do not retry blindly: %w", err)
	}
	return receipt, nil
}

func newMediaClientID() (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d_%s", time.Now().UnixNano()/int64(time.Millisecond), hex.EncodeToString(nonce[:])), nil
}
