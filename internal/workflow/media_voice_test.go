package workflow

import (
	"bytes"
	"errors"
	"testing"

	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func TestVoiceRejectsUnencodedAndTruncatedAudio(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("RIFF fake WAV"), []byte("#!AMR\n"), []byte("#!AMR\n\x3c"), []byte("\x02#!SILK_V3\x04\x00x")} {
		if _, err := VoiceFormat(data); err == nil {
			t.Fatalf("accepted invalid voice container %q", data)
		}
	}
	amr := append([]byte("#!AMR\n\x3c"), make([]byte, 31)...)
	if format, err := VoiceFormat(amr); err != nil || format != 0 {
		t.Fatalf("AMR framing rejected: %d %v", format, err)
	}
	if format, err := VoiceFormat([]byte("\x02#!SILK_V3\x02\x00ab")); err != nil || format != 4 {
		t.Fatalf("SILK framing rejected: %d %v", format, err)
	}
	if _, err := VoiceFormat([]byte("\x02#!SILK_V3\x0a\x001234567890")); err != nil {
		t.Fatalf("10-byte SILK frame length mistaken for a newline: %v", err)
	}
}

func TestVoiceChunksEndExactlyOnceAndRejectBadAcknowledgements(t *testing.T) {
	for _, size := range []int{1, voiceChunkSize, voiceChunkSize + 1} {
		calls := 0
		data := bytes.Repeat([]byte{7}, size)
		r, err := sendVoiceChunks(&wechat.BaseRequest{}, "from", "to", data, 1000, 4, "client", func(path string, cgi uint32, request, response proto.Message) error {
			calls++
			req := request.(*wechat.UploadVoiceRequest)
			if path != "/cgi-bin/micromsg-bin/uploadvoice" || cgi != 329 || req.GetVoiceLength() != 1000 || req.GetLength() == 0 {
				t.Fatal("wrong voice request contract")
			}
			end := int(req.GetOffset() + req.GetLength())
			if (req.GetEndFlag() == 1) != (end == size) || !bytes.Equal(req.GetData().GetBuffer(), data[int(req.GetOffset()):end]) {
				t.Fatal("wrong voice data or final chunk")
			}
			*response.(*wechat.UploadVoiceResponse) = voiceTestAck(req)
			if end == size {
				// Observed in the real filehelper acknowledgement on 2026-10-23.
				response.(*wechat.UploadVoiceResponse).EndFlag = proto.Uint32(2)
			}
			return nil
		})
		if err != nil || !r.ServerAccepted || calls != (size+voiceChunkSize-1)/voiceChunkSize || r.MessageID != "9007199254740993" {
			t.Fatalf("unexpected voice result: %#v %v %d", r, err, calls)
		}
	}
	for _, fault := range []string{"ret", "identity", "offset", "end", "unknown-end", "cancel", "id", "transport"} {
		t.Run(fault, func(t *testing.T) {
			calls := 0
			r, err := sendVoiceChunks(&wechat.BaseRequest{}, "from", "to", []byte{1}, 1000, 4, "client", func(_ string, _ uint32, request, response proto.Message) error {
				calls++
				resp := voiceTestAck(request.(*wechat.UploadVoiceRequest))
				switch fault {
				case "ret":
					resp.BaseResponse.Ret = proto.Int32(-1)
				case "identity":
					resp.ToUserName = proto.String("other")
				case "offset":
					resp.Offset = proto.Uint32(99)
				case "end":
					resp.EndFlag = nil
				case "unknown-end":
					resp.EndFlag = proto.Uint32(3)
				case "cancel":
					resp.CancelFlag = proto.Uint32(1)
				case "id":
					resp.NewMsgId = nil
				case "transport":
					return errors.New("connection lost")
				}
				*response.(*wechat.UploadVoiceResponse) = resp
				return nil
			})
			if err == nil || calls != 1 || r.ServerAccepted {
				t.Fatalf("bad acknowledgement accepted or retried: %#v %v %d", r, err, calls)
			}
		})
	}
}

func voiceTestAck(req *wechat.UploadVoiceRequest) wechat.UploadVoiceResponse {
	return wechat.UploadVoiceResponse{BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(0)},
		FromUserName: req.FromUserName, ToUserName: req.ToUserName, ClientMsgId: req.ClientMsgId,
		Offset: proto.Uint32(req.GetOffset() + req.GetLength()), Length: req.Length, EndFlag: req.EndFlag,
		NewMsgId: proto.Uint64(9007199254740993)}
}
