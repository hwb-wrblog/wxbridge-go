package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"strconv"
	"time"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

// MaxImageBytes and the pixel limit are local resource limits, not recovered
// server limits. JPEG input is normalized to the target's PNG upload path.
const MaxImageBytes = 20 << 20

const imageMaxPixels = 20_000_000
const imageChunkSize = 50_000

type imageBoundedBuffer struct{ bytes.Buffer }

func (b *imageBoundedBuffer) Write(p []byte) (int, error) {
	if len(p) > MaxImageBytes-b.Len() {
		return 0, errors.New("PNG conversion exceeds image byte limit")
	}
	return b.Buffer.Write(p)
}

func prepareImageBytes(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > MaxImageBytes {
		return nil, fmt.Errorf("image must contain 1 to %d bytes", MaxImageBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid image header: %w", err)
	}
	if format != "png" && format != "jpeg" {
		return nil, errors.New("image upload supports PNG and JPEG; GIF sticker upload is not recovered")
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > imageMaxPixels {
		return nil, fmt.Errorf("image dimensions exceed %d pixels", imageMaxPixels)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid image data: %w", err)
	}
	if format == "png" {
		return data, nil
	}
	var converted imageBoundedBuffer
	if err := png.Encode(&converted, decoded); err != nil {
		return nil, fmt.Errorf("convert JPEG to PNG: %w", err)
	}
	return converted.Bytes(), nil
}

// SendImage implements target SendUploadImageNewRequest (0xb284a0), CGI 110.
// It validates each response and omits the target's redundant empty last chunk.
func (s *Session) SendImage(to string, data []byte) (*MediaReceipt, error) {
	payload, err := prepareImageBytes(data)
	if err != nil {
		return nil, err
	}
	if err := s.prepareMedia(to); err != nil {
		return nil, err
	}
	u := s.State.User
	if u.DeviceInfo == nil || u.WxId == "" {
		return nil, errors.New("image upload requires an authenticated iPad identity")
	}
	clientID, err := newMediaClientID()
	if err != nil {
		return nil, err
	}
	receipt, err := sendImageChunks(clientsdk.GetBaseRequest(u), u.WxId, to, payload, clientID, time.Now(), s.exchangeMedia)
	if err != nil {
		return receipt, err
	}
	return s.finishMedia(receipt)
}

func sendImageChunks(base *wechat.BaseRequest, from, to string, data []byte, clientID string, now time.Time, exchange mediaExchange) (*MediaReceipt, error) {
	if len(data) == 0 || len(data) > MaxImageBytes || base == nil || from == "" || to == "" || clientID == "" || exchange == nil {
		return nil, errors.New("invalid image upload parameters")
	}
	receipt := &MediaReceipt{Kind: "image", Recipient: to, ClientID: clientID, Bytes: len(data)}
	deadline := time.Now().Add(2 * time.Minute)
	var previousLegacyID uint32
	var previousNewID int64
	for start := 0; start < len(data); start += imageChunkSize {
		if time.Now().After(deadline) {
			return receipt, errors.New("image upload deadline exceeded; do not retry blindly")
		}
		end := start + imageChunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[start:end]
		req := &wechat.UploadMsgImgRequest{
			BaseRequest: base,
			ClientImgId: &wechat.SKBuiltinString{Str: proto.String(clientID)},
			SenderWxid:  &wechat.SKBuiltinString{Str: proto.String(from)},
			RecvWxid:    &wechat.SKBuiltinString{Str: proto.String(to)},
			TotalLen:    proto.Uint32(uint32(len(data))),
			StartPos:    proto.Uint32(uint32(start)),
			DataLen:     proto.Uint32(uint32(len(chunk))),
			Data:        &wechat.SKBuiltinString_{Len: proto.Uint32(uint32(len(chunk))), Buffer: chunk},
			MsgType:     proto.Uint32(3),
			EncryVer:    proto.Uint32(0),
			ReqTime:     proto.Uint32(uint32(now.Unix())),
			MessageExt:  proto.String("png"),
		}
		var resp wechat.UploadMsgImgResponse
		if err := exchange("/cgi-bin/micromsg-bin/uploadmsgimg", 110, req, &resp); err != nil {
			return receipt, fmt.Errorf("image chunk at %d failed without retry: %w", start, err)
		}
		id, err := validateImageAck(&resp, from, to, clientID, len(data), start, end)
		if err != nil {
			return receipt, fmt.Errorf("image chunk at %d was not acknowledged: %w", start, err)
		}
		if legacyID := resp.GetMsgId(); legacyID != 0 {
			if previousLegacyID != 0 && previousLegacyID != legacyID {
				return receipt, errors.New("image response changed legacy message ID")
			}
			previousLegacyID = legacyID
		}
		if newID := resp.GetNewMsgId(); newID != 0 {
			if previousNewID != 0 && previousNewID != newID {
				return receipt, errors.New("image response changed server message ID")
			}
			previousNewID = newID
		}
		receipt.Chunks++
		if end == len(data) {
			if id == "" {
				return receipt, errors.New("image upload completed without a server message ID; do not retry blindly")
			}
			receipt.MessageID, receipt.ServerAccepted = id, true
		}
	}
	return receipt, nil
}

func validateImageAck(resp *wechat.UploadMsgImgResponse, from, to, clientID string, total, start, end int) (string, error) {
	if err := responseOK(resp.GetBaseResponse()); err != nil {
		return "", err
	}
	if resp.GetClientImgId().GetStr() != clientID || resp.GetToUserName().GetStr() != to {
		return "", errors.New("image acknowledgement client or recipient mismatch")
	}
	if sender := resp.GetFromUserName().GetStr(); sender != "" && sender != from {
		return "", errors.New("image acknowledgement sender mismatch")
	}
	if resp.TotalLen == nil || int64(resp.GetTotalLen()) != int64(total) || resp.StartPos == nil {
		return "", errors.New("image acknowledgement lacks matching total length and offset")
	}
	position, count := int64(resp.GetStartPos()), int64(resp.GetDataLen())
	// Accept a next-offset acknowledgement or an explicitly acknowledged range.
	// Never infer progress from a zero result code alone.
	nextOffset := position == int64(end) && (resp.DataLen == nil || count == 0 || count == int64(end-start))
	rangeAck := position == int64(start) && resp.DataLen != nil && count == int64(end-start)
	if !nextOffset && !rangeAck {
		return "", errors.New("image acknowledgement is partial, stalled, or outside the sent chunk")
	}
	if id := resp.GetNewMsgId(); id != 0 {
		return strconv.FormatUint(uint64(id), 10), nil
	}
	if id := resp.GetMsgId(); id != 0 {
		return strconv.FormatUint(uint64(id), 10), nil
	}
	return "", nil
}
