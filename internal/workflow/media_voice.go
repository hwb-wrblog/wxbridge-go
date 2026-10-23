package workflow

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

const MaxVoiceBytes = 1 << 20
const voiceChunkSize = 65000

// VoiceFormat validates the encoded container. Encoding/decoding audio is left
// to a codec such as SILK or OpenCORE AMR, not implemented by this workflow.
func VoiceFormat(data []byte) (int32, error) {
	if len(data) == 0 || len(data) > MaxVoiceBytes {
		return 0, errors.New("voice must contain 1 to 1048576 bytes")
	}
	if bytes.HasPrefix(data, []byte("#!AMR\n")) {
		frameBytes := []int{13, 14, 16, 18, 20, 21, 27, 32, 6, 0, 0, 0, 0, 0, 0, 1}
		offset, frames := 6, 0
		for offset < len(data) {
			header := data[offset]
			n := frameBytes[(header>>3)&15]
			if header&0x83 != 0 || n == 0 || n > len(data)-offset {
				return 0, errors.New("invalid or truncated AMR frame")
			}
			offset += n
			frames++
		}
		if frames == 0 || frames > 3000 {
			return 0, errors.New("AMR must contain audio lasting no more than 60 seconds")
		}
		return 0, nil
	}
	stream := data
	if stream[0] == 2 {
		stream = stream[1:]
	}
	if !bytes.HasPrefix(stream, []byte("#!SILK_V3")) {
		return 0, errors.New("voice requires AMR-NB or SILK V3 encoded data; WAV and MP3 are not voice payloads")
	}
	stream = stream[len("#!SILK_V3"):]
	frames := 0
	for len(stream) > 0 {
		if len(stream) < 2 {
			return 0, errors.New("truncated SILK frame length")
		}
		n := int(int16(binary.LittleEndian.Uint16(stream)))
		stream = stream[2:]
		if n == -1 && len(stream) == 0 {
			break
		}
		if n <= 0 || n > 1024 || n > len(stream) {
			return 0, errors.New("invalid or truncated SILK frame")
		}
		stream = stream[n:]
		frames++
	}
	if frames == 0 {
		return 0, errors.New("SILK container contains no audio")
	}
	return 4, nil
}

func (s *Session) SendVoice(to string, data []byte, durationMS int32) (*MediaReceipt, error) {
	format, err := VoiceFormat(data)
	if err != nil {
		return nil, err
	}
	if durationMS < 1 || durationMS > 60000 {
		return nil, errors.New("voice duration must be between 1 and 60000 milliseconds")
	}
	if err := s.prepareMedia(to); err != nil {
		return nil, err
	}
	clientID, err := newMediaClientID()
	if err != nil {
		return nil, err
	}
	r, err := sendVoiceChunks(clientsdk.GetBaseRequest(s.State.User), s.State.User.WxId, to, data, durationMS, format, clientID, s.exchangeMedia)
	if err != nil {
		return r, err
	}
	return s.finishMedia(r)
}

// Reconstructed from SendUploadVoiceNewRequest (0xb375a0), CGI 329.
func sendVoiceChunks(base *wechat.BaseRequest, from, to string, data []byte, durationMS, format int32, clientID string, exchange mediaExchange) (*MediaReceipt, error) {
	if base == nil || from == "" || to == "" || clientID == "" || exchange == nil || len(data) == 0 || len(data) > MaxVoiceBytes || durationMS < 1 || durationMS > 60000 || (format != 0 && format != 4) {
		return nil, errors.New("invalid voice upload parameters")
	}
	r := &MediaReceipt{Kind: "voice", Recipient: to, ClientID: clientID, Bytes: len(data)}
	deadline := time.Now().Add(2 * time.Minute)
	var previousLegacyID uint32
	var previousNewID uint64
	for start := 0; start < len(data); start += voiceChunkSize {
		if time.Now().After(deadline) {
			return r, errors.New("voice upload deadline exceeded; do not retry blindly")
		}
		end := start + voiceChunkSize
		if end > len(data) {
			end = len(data)
		}
		final := end == len(data)
		endFlag := uint32(0)
		if final {
			endFlag = 1
		}
		req := &wechat.UploadVoiceRequest{
			BaseRequest: base, FromUserName: proto.String(from), ToUserName: proto.String(to),
			Offset: proto.Uint32(uint32(start)), Length: proto.Uint32(uint32(end - start)),
			ClientMsgId: proto.String(clientID), MsgId: proto.Uint32(0),
			VoiceLength: proto.Int32(durationMS), VoiceFormat: proto.Int32(format),
			Data:    &wechat.SKBuiltinString_{Len: proto.Uint32(uint32(end - start)), Buffer: data[start:end]},
			EndFlag: proto.Uint32(endFlag),
		}
		var resp wechat.UploadVoiceResponse
		if err := exchange("/cgi-bin/micromsg-bin/uploadvoice", 329, req, &resp); err != nil {
			return r, fmt.Errorf("voice chunk at %d failed without retry: %w", start, err)
		}
		if err := responseOK(resp.GetBaseResponse()); err != nil {
			return r, err
		}
		if resp.GetToUserName() != to || resp.GetClientMsgId() != clientID || (resp.FromUserName != nil && resp.GetFromUserName() != from) {
			return r, errors.New("voice acknowledgement identity mismatch")
		}
		if resp.GetCancelFlag() != 0 {
			return r, errors.New("voice upload cancelled by server")
		}
		position, count := int64(resp.GetOffset()), int64(resp.GetLength())
		nextAck := position == int64(end) && (resp.Length == nil || count == 0 || count == int64(end-start))
		rangeAck := position == int64(start) && resp.Length != nil && count == int64(end-start)
		if resp.Offset == nil || (!nextAck && !rangeAck) {
			return r, errors.New("voice acknowledgement has missing or inconsistent progress")
		}
		// Live 8.0.75 replies use 2 for a completed voice message. The target
		// invoker only checked 1; both require full progress and a message ID.
		completionOK := (!final && resp.GetEndFlag() == 0) || (final && (resp.GetEndFlag() == 1 || resp.GetEndFlag() == 2))
		if resp.EndFlag == nil || !completionOK {
			return r, fmt.Errorf("voice acknowledgement has inconsistent completion flag (present=%t, value=%d, expected=%d, new_message_id=%d, message_id=%d)", resp.EndFlag != nil, resp.GetEndFlag(), endFlag, resp.GetNewMsgId(), resp.GetMsgId())
		}
		id := ""
		if resp.GetNewMsgId() != 0 {
			id = strconv.FormatUint(resp.GetNewMsgId(), 10)
		} else if resp.GetMsgId() != 0 {
			id = strconv.FormatUint(uint64(resp.GetMsgId()), 10)
		}
		if newID := resp.GetNewMsgId(); newID != 0 {
			if previousNewID != 0 && previousNewID != newID {
				return r, errors.New("voice acknowledgement changed message ID")
			}
			previousNewID = newID
		}
		if legacyID := resp.GetMsgId(); legacyID != 0 {
			if previousLegacyID != 0 && previousLegacyID != legacyID {
				return r, errors.New("voice acknowledgement changed legacy message ID")
			}
			previousLegacyID = legacyID
		}
		r.Chunks++
		if final {
			if id == "" {
				return r, errors.New("voice completion has no server message ID; do not retry blindly")
			}
			r.MessageID, r.ServerAccepted = id, true
		}
	}
	return r, nil
}
