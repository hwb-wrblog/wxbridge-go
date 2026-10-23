package workflow

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

// SendEmoji references a sticker already stored by the server; it does not
// upload GIF bytes. Target SendEmojiRequest (0xcd4740) calls SendAppMessage
// with type 8, which uses /sendappmsg and CGI 222 at 0xb2e3c0.
func (s *Session) SendEmoji(to, md5 string, size int32) (*MediaReceipt, error) {
	md5, err := validateEmojiReference(md5, size)
	if err != nil {
		return nil, err
	}
	if err := s.prepareMedia(to); err != nil {
		return nil, err
	}
	u := s.State.User
	if u.DeviceInfo == nil || u.WxId == "" {
		return nil, errors.New("emoji sending requires an authenticated iPad identity")
	}
	clientID, err := newMediaClientID()
	if err != nil {
		return nil, err
	}
	receipt, err := sendEmojiReference(clientsdk.GetBaseRequest(u), u.WxId, to, md5, size, clientID, time.Now(), s.exchangeMedia)
	if err != nil {
		return receipt, err
	}
	return s.finishMedia(receipt)
}

func validateEmojiReference(md5 string, size int32) (string, error) {
	if len(md5) != 32 {
		return "", errors.New("emoji MD5 must be exactly 32 hexadecimal characters")
	}
	if _, err := hex.DecodeString(md5); err != nil {
		return "", errors.New("emoji MD5 must be exactly 32 hexadecimal characters")
	}
	if size <= 0 {
		return "", errors.New("emoji size must be a positive int32 byte count")
	}
	return strings.ToLower(md5), nil
}

func sendEmojiReference(base *wechat.BaseRequest, from, to, md5 string, size int32, clientID string, now time.Time, exchange mediaExchange) (*MediaReceipt, error) {
	md5, err := validateEmojiReference(md5, size)
	if err != nil {
		return nil, err
	}
	if base == nil || from == "" || to == "" || clientID == "" || exchange == nil {
		return nil, errors.New("invalid emoji request parameters")
	}
	req := &wechat.SendAppMsgRequest{
		BaseRequest: base,
		Msg: &wechat.AppMsg{
			FromUserName: proto.String(from), AppId: proto.String(""), SdkVersion: proto.Uint32(0),
			ToUserName: proto.String(to), Type: proto.Uint32(8),
			Content:    proto.String(clientsdk.CreateSendEmojiMsgXMl(md5, size)),
			CreateTime: proto.Uint32(uint32(now.Unix())), ClientMsgId: proto.String(clientID),
			Source: proto.Int32(0), RemindId: proto.Int32(0), MsgSource: proto.String(""),
			Thumb: &wechat.BufferT{ILen: proto.Uint32(0), Buffer: []byte{}},
		},
		// Target stores value 2 at offset 0x50: MsgForwardType, not DirectShare.
		MsgForwardType: proto.Uint32(2),
		SendMsgTicket:  proto.String(""),
	}
	receipt := &MediaReceipt{Kind: "emoji", Recipient: to, ClientID: clientID, Bytes: int(size)}
	var resp wechat.SendAppMsgResponse
	if err := exchange("/cgi-bin/micromsg-bin/sendappmsg", 222, req, &resp); err != nil {
		return receipt, fmt.Errorf("emoji request failed without retry: %w", err)
	}
	if err := responseOK(resp.GetBaseResponse()); err != nil {
		return receipt, err
	}
	if resp.GetToUserName() != to || resp.GetClientMsgId() != clientID {
		return receipt, errors.New("emoji acknowledgement client or recipient mismatch")
	}
	if resp.FromUserName != nil && resp.GetFromUserName() != from {
		return receipt, errors.New("emoji acknowledgement sender mismatch")
	}
	if resp.Type != nil && resp.GetType() != 8 {
		return receipt, errors.New("emoji acknowledgement message type mismatch")
	}
	if id := resp.GetNewMsgId(); id != 0 {
		receipt.MessageID = strconv.FormatUint(id, 10)
	} else if id := resp.GetMsgId(); id != 0 {
		receipt.MessageID = strconv.FormatUint(uint64(id), 10)
	} else {
		return receipt, errors.New("emoji response has no server message ID; do not retry blindly")
	}
	receipt.Chunks, receipt.ServerAccepted = 1, true
	return receipt, nil
}
