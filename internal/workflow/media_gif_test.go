package workflow

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"strconv"
	"strings"
	"testing"

	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func gifTestData(t *testing.T, frames int) []byte {
	t.Helper()
	var data bytes.Buffer
	animation := &gif.GIF{LoopCount: 0}
	for i := 0; i < frames; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
		frame.Pix[i%len(frame.Pix)] = 1
		animation.Image = append(animation.Image, frame)
		animation.Delay = append(animation.Delay, i+1)
	}
	if err := gif.EncodeAll(&data, animation); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

// A small structurally valid container with no compressed image data lets the
// preflight budget checks be tested without allocating enormous decoded frames.
func gifTestContainer(width, height, frames int) []byte {
	header := []byte("GIF89a\x00\x00\x00\x00\x00\x00\x00")
	binary.LittleEndian.PutUint16(header[6:8], uint16(width))
	binary.LittleEndian.PutUint16(header[8:10], uint16(height))
	for i := 0; i < frames; i++ {
		frame := []byte{0x2c, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0}
		binary.LittleEndian.PutUint16(frame[5:7], uint16(width))
		binary.LittleEndian.PutUint16(frame[7:9], uint16(height))
		header = append(header, frame...)
	}
	return append(header, 0x3b)
}

func TestMediaGIFValidationAndAllocationBudgets(t *testing.T) {
	data := gifTestData(t, 3)
	before := append([]byte(nil), data...)
	if err := validateGIFBytes(data); err != nil || !bytes.Equal(data, before) {
		t.Fatalf("animation rejected or changed: %v", err)
	}
	static := gifTestData(t, 1)
	copy(static[:6], "GIF87a")
	if err := validateGIFBytes(static); err != nil {
		t.Fatalf("GIF87a rejected: %v", err)
	}
	for n := 0; n < len(data); n++ {
		if err := validateGIFBytes(data[:n]); err == nil {
			t.Fatalf("truncated GIF accepted at byte %d", n)
		}
	}
	for name, bad := range map[string][]byte{
		"wrong format":            []byte("not GIF"),
		"byte cap":                make([]byte, MaxGIFBytes+1),
		"canvas cap":              gifTestContainer(2001, 2000, 1),
		"frame cap":               gifTestContainer(1, 1, MaxGIFFrames+1),
		"pixel cap":               gifTestContainer(2000, 2000, 6),
		"zero width":              gifTestContainer(0, 1, 1),
		"no frames":               gifTestContainer(1, 1, 0),
		"invalid compressed data": gifTestContainer(1, 1, 1),
		"trailing bytes":          append(append([]byte(nil), data...), 0),
		"unknown block":           append(append([]byte(nil), data[:len(data)-1]...), 0x12, 0x3b),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateGIFBytes(bad); err == nil {
				t.Fatal("invalid GIF accepted")
			}
		})
	}
	for name, data := range map[string][]byte{
		"frames":            gifTestContainer(1, 1, MaxGIFFrames+1),
		"cumulative pixels": gifTestContainer(2000, 2000, 6),
	} {
		t.Run(name+" before decoding", func(t *testing.T) {
			if _, err := preflightGIF(data); err == nil || !strings.Contains(err.Error(), "cumulative frame pixels") {
				t.Fatalf("allocation budget not enforced in preflight: %v", err)
			}
		})
	}
	for _, change := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint16(b[14:16], 1) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[16:18], 1) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[18:20], 0) },
		func(b []byte) { b[10] = 0x87 },
		func(b []byte) { b[22] = 0x87 },
	} {
		bad := gifTestContainer(1, 1, 1)
		change(bad)
		if _, err := preflightGIF(bad); err == nil {
			t.Fatal("invalid frame rectangle or truncated color table accepted")
		}
	}
	if _, err := (&Session{}).SendGIF("receiver", data); err == nil || err.Error() != "not authenticated" {
		t.Fatalf("unauthenticated GIF was not rejected: %v", err)
	}
}

func gifTestAck(req *wechat.UploadEmojiRequest) *wechat.UploadEmojiResponse {
	item := req.EmojiItem[0]
	return &wechat.UploadEmojiResponse{
		BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(0)}, EmojiItemCount: proto.Int32(1),
		EmojiItem: []*wechat.EmojiUploadInfoResp{{
			Ret: proto.Int32(0), StartPos: proto.Int32(item.GetStartPos() + int32(item.GetEmojiBuffer().GetILen())),
			TotalLen: item.TotalLen, MD5: item.MD5, MsgID: proto.Uint32(0),
		}},
	}
}

func TestMediaGIFChunksPreserveBytesAndIdentity(t *testing.T) {
	inputs := map[string][]byte{"animation": gifTestData(t, 3)}
	for _, size := range []int{1, gifChunkSize - 1, gifChunkSize, gifChunkSize + 1, 2 * gifChunkSize, 2*gifChunkSize + 1, MaxGIFBytes} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i*17 + 2)
		}
		inputs[strconv.Itoa(size)] = data
	}
	for name, data := range inputs {
		t.Run(name, func(t *testing.T) {
			calls, next := 0, 0
			base := &wechat.BaseRequest{}
			wantMD5 := fmt.Sprintf("%x", md5.Sum(data))
			newID := ^uint64(0) - 7
			exchange := func(path string, cgi uint32, request, response proto.Message) error {
				calls++
				if path != "/cgi-bin/micromsg-bin/sendemoji" || cgi != 175 {
					t.Fatalf("unexpected endpoint or CGI: %s, %d", path, cgi)
				}
				req := request.(*wechat.UploadEmojiRequest)
				if req.BaseRequest != base || req.GetEmojiItemCount() != 1 || len(req.EmojiItem) != 1 {
					t.Fatal("invalid upload envelope")
				}
				item := req.EmojiItem[0]
				buffer := item.GetEmojiBuffer()
				end := next + gifChunkSize
				if end > len(data) {
					end = len(data)
				}
				if item.GetStartPos() != int32(next) || item.GetTotalLen() != int32(len(data)) || buffer.GetILen() != uint32(end-next) || buffer.GetILen() == 0 || !bytes.Equal(buffer.GetBuffer(), data[next:end]) {
					t.Fatal("GIF bytes or chunk boundaries changed")
				}
				if item.GetMD5() != wantMD5 || item.GetToUserName() != "receiver" || item.GetClientMsgID() != "gif-client" || item.GetType() != 1 {
					t.Fatal("GIF identity or target Type changed between chunks")
				}
				wire, err := proto.Marshal(item)
				if err != nil {
					t.Fatalf("request item lacks required protobuf fields: %v", err)
				}
				var decoded wechat.EmojiUploadInfoReq
				if err := proto.Unmarshal(wire, &decoded); err != nil || !bytes.Equal(decoded.GetEmojiBuffer().GetBuffer(), data[next:end]) {
					t.Fatalf("GIF bytes lost during serialization: %v", err)
				}
				ack := gifTestAck(req)
				ack.EmojiItem[0].MsgID = proto.Uint32(31)
				if end == len(data) {
					ack.EmojiItem[0].NewMsgId = proto.Uint64(newID)
					ack.EmojiItem[0].MD5 = proto.String(strings.ToUpper(wantMD5))
				}
				*response.(*wechat.UploadEmojiResponse) = *ack
				next = end
				return nil
			}
			receipt, err := sendGIFChunks(base, "receiver", data, "gif-client", exchange)
			wantCalls := (len(data) + gifChunkSize - 1) / gifChunkSize
			if err != nil || calls != wantCalls || receipt.Chunks != wantCalls || !receipt.ServerAccepted || !receipt.UploadComplete || receipt.MessageID != strconv.FormatUint(newID, 10) || receipt.Bytes != len(data) || receipt.Kind != "gif" {
				t.Fatalf("incorrect GIF result: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
			}
		})
	}
}

func TestMediaGIFRejectsInvalidAcknowledgementsWithoutRetry(t *testing.T) {
	for name, mutate := range map[string]func(*wechat.UploadEmojiResponse){
		"missing result":            func(r *wechat.UploadEmojiResponse) { r.BaseResponse = nil },
		"missing outer ret":         func(r *wechat.UploadEmojiResponse) { r.BaseResponse.Ret = nil },
		"outer error":               func(r *wechat.UploadEmojiResponse) { r.BaseResponse.Ret = proto.Int32(-1) },
		"missing count":             func(r *wechat.UploadEmojiResponse) { r.EmojiItemCount = nil },
		"wrong count":               func(r *wechat.UploadEmojiResponse) { r.EmojiItemCount = proto.Int32(2) },
		"missing item":              func(r *wechat.UploadEmojiResponse) { r.EmojiItem = nil },
		"nil item":                  func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0] = nil },
		"extra item":                func(r *wechat.UploadEmojiResponse) { r.EmojiItem = append(r.EmojiItem, r.EmojiItem[0]) },
		"missing item ret":          func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].Ret = nil },
		"item error":                func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].Ret = proto.Int32(-2) },
		"missing MD5":               func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].MD5 = nil },
		"wrong MD5":                 func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].MD5 = proto.String(strings.Repeat("0", 32)) },
		"missing length":            func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].TotalLen = nil },
		"wrong length":              func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].TotalLen = proto.Int32(1) },
		"negative length":           func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].TotalLen = proto.Int32(-1) },
		"missing offset":            func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].StartPos = nil },
		"negative offset":           func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].StartPos = proto.Int32(-1) },
		"stalled offset":            func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].StartPos = proto.Int32(0) },
		"partial offset":            func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].StartPos = proto.Int32(gifChunkSize - 1) },
		"jumping offset":            func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].StartPos = proto.Int32(gifChunkSize + 1) },
		"missing required ID field": func(r *wechat.UploadEmojiResponse) { r.EmojiItem[0].MsgID = nil },
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			exchange := func(_ string, _ uint32, request, response proto.Message) error {
				calls++
				ack := gifTestAck(request.(*wechat.UploadEmojiRequest))
				mutate(ack)
				*response.(*wechat.UploadEmojiResponse) = *ack
				return nil
			}
			receipt, err := sendGIFChunks(&wechat.BaseRequest{}, "receiver", make([]byte, gifChunkSize+1), "client", exchange)
			if err == nil || calls != 1 || receipt == nil || receipt.Chunks != 0 || receipt.ServerAccepted || receipt.UploadComplete {
				t.Fatalf("invalid response accepted or retried: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
			}
		})
	}
}

func TestMediaGIFStopsAtTransportFailureWithoutRetry(t *testing.T) {
	for failAt := 1; failAt <= 3; failAt++ {
		calls := 0
		failure := errors.New("offline transport failure")
		exchange := func(_ string, _ uint32, request, response proto.Message) error {
			calls++
			if calls == failAt {
				return failure
			}
			*response.(*wechat.UploadEmojiResponse) = *gifTestAck(request.(*wechat.UploadEmojiRequest))
			return nil
		}
		receipt, err := sendGIFChunks(&wechat.BaseRequest{}, "receiver", make([]byte, 2*gifChunkSize+1), "client", exchange)
		if !errors.Is(err, failure) || calls != failAt || receipt.Chunks != failAt-1 || receipt.ServerAccepted || receipt.UploadComplete {
			t.Fatalf("failed upload advanced or retried: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
		}
	}
}

func TestMediaGIFMissingFinalMessageIDIsUnknownDelivery(t *testing.T) {
	calls := 0
	exchange := func(_ string, _ uint32, request, response proto.Message) error {
		calls++
		ack := gifTestAck(request.(*wechat.UploadEmojiRequest))
		if calls == 1 {
			ack.EmojiItem[0].MsgID = proto.Uint32(7)
			ack.EmojiItem[0].NewMsgId = proto.Uint64(900)
		}
		*response.(*wechat.UploadEmojiResponse) = *ack
		return nil
	}
	receipt, err := sendGIFChunks(&wechat.BaseRequest{}, "receiver", make([]byte, gifChunkSize+1), "client", exchange)
	if err == nil || !strings.Contains(err.Error(), "delivery is unknown") || calls != 2 || receipt.Chunks != 2 || receipt.ServerAccepted || !receipt.UploadComplete || receipt.MessageID != "" {
		t.Fatalf("missing final ID treated as delivery or followed by another send: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
	}
}

func TestMediaGIFMessageIDsRemainConsistent(t *testing.T) {
	for _, newID := range []bool{false, true} {
		calls := 0
		exchange := func(_ string, _ uint32, request, response proto.Message) error {
			calls++
			ack := gifTestAck(request.(*wechat.UploadEmojiRequest))
			if newID {
				ack.EmojiItem[0].NewMsgId = proto.Uint64(uint64(calls))
			} else {
				ack.EmojiItem[0].MsgID = proto.Uint32(uint32(calls))
			}
			*response.(*wechat.UploadEmojiResponse) = *ack
			return nil
		}
		receipt, err := sendGIFChunks(&wechat.BaseRequest{}, "receiver", make([]byte, 2*gifChunkSize+1), "client", exchange)
		if err == nil || calls != 2 || receipt.Chunks != 1 || receipt.ServerAccepted || receipt.UploadComplete {
			t.Fatalf("changed ID accepted or upload continued: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
		}
	}
	exchange := func(_ string, _ uint32, request, response proto.Message) error {
		ack := gifTestAck(request.(*wechat.UploadEmojiRequest))
		ack.EmojiItem[0].MsgID = proto.Uint32(42)
		*response.(*wechat.UploadEmojiResponse) = *ack
		return nil
	}
	receipt, err := sendGIFChunks(&wechat.BaseRequest{}, "receiver", []byte{1}, "client", exchange)
	if err != nil || receipt.MessageID != "42" || !receipt.ServerAccepted {
		t.Fatalf("legacy-only final ID rejected: %+v, %v", receipt, err)
	}
}
