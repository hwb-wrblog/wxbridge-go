package workflow

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"

	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func imageTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 100, G: 40, B: 210, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestMediaImageValidationAndJPEGNormalization(t *testing.T) {
	input := imageTestPNG(t)
	got, err := prepareImageBytes(input)
	if err != nil || !bytes.Equal(input, got) {
		t.Fatalf("PNG was rejected or changed: %v", err)
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 3, 2)), nil); err != nil {
		t.Fatal(err)
	}
	got, err = prepareImageBytes(jpg.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(got))
	if err != nil || format != "png" || config.Width != 3 || config.Height != 2 {
		t.Fatalf("JPEG was not converted to PNG: %s, %+v, %v", format, config, err)
	}
	huge := append([]byte(nil), input...)
	binary.BigEndian.PutUint32(huge[16:20], imageMaxPixels+1)
	binary.BigEndian.PutUint32(huge[20:24], 1)
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	for name, bad := range map[string][]byte{
		"empty": nil, "unknown": []byte("not an image"), "truncated": input[:len(input)-1],
		"too many pixels": huge, "too many bytes": make([]byte, MaxImageBytes+1),
		"gif": []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareImageBytes(bad); err == nil {
				t.Fatal("invalid image accepted")
			}
		})
	}
	if _, err := (&Session{}).SendImage("receiver", input); err == nil {
		t.Fatal("image sent without authentication")
	}
}

func imageTestAck(req *wechat.UploadMsgImgRequest) *wechat.UploadMsgImgResponse {
	return &wechat.UploadMsgImgResponse{
		BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(0)},
		ClientImgId:  req.ClientImgId, FromUserName: req.SenderWxid, ToUserName: req.RecvWxid,
		TotalLen: req.TotalLen, StartPos: proto.Uint32(req.GetStartPos() + req.GetDataLen()), DataLen: req.DataLen,
	}
}

func TestMediaImageChunksAndTargetRequestFields(t *testing.T) {
	for _, size := range []int{1, 49999, 50000, 50001, 100000, 100001} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i * 37)
			}
			calls := 0
			base := &wechat.BaseRequest{Uin: proto.Uint32(7)}
			now := time.Unix(1700000000, 0)
			exchange := func(path string, cgi uint32, request, response proto.Message) error {
				if path != "/cgi-bin/micromsg-bin/uploadmsgimg" || cgi != 110 {
					t.Fatalf("unexpected transport: %s %d", path, cgi)
				}
				req := request.(*wechat.UploadMsgImgRequest)
				start := calls * imageChunkSize
				end := start + imageChunkSize
				if end > size {
					end = size
				}
				calls++
				if req.BaseRequest != base || req.GetStartPos() != uint32(start) || req.GetTotalLen() != uint32(size) || req.GetDataLen() != uint32(end-start) || req.GetDataLen() == 0 {
					t.Fatal("invalid image chunk boundaries")
				}
				if req.GetData().GetLen() != uint32(end-start) || !bytes.Equal(req.GetData().GetBuffer(), data[start:end]) {
					t.Fatal("image chunk payload mismatch")
				}
				if req.GetMsgType() != 3 || req.GetEncryVer() != 0 || req.EncryVer == nil || req.GetMessageExt() != "png" || req.GetReqTime() != uint32(now.Unix()) {
					t.Fatal("target image request fields changed")
				}
				if req.GetClientImgId().GetStr() != "image-client" || req.GetSenderWxid().GetStr() != "sender" || req.GetRecvWxid().GetStr() != "receiver" {
					t.Fatal("image identity changed between chunks")
				}
				ack := imageTestAck(req)
				if end == size {
					ack.NewMsgId = proto.Int64(1<<60 + 17)
				}
				*response.(*wechat.UploadMsgImgResponse) = *ack
				return nil
			}
			receipt, err := sendImageChunks(base, "sender", "receiver", data, "image-client", now, exchange)
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := (size + imageChunkSize - 1) / imageChunkSize
			if calls != wantCalls || receipt.Chunks != wantCalls || !receipt.ServerAccepted || receipt.MessageID != "1152921504606846993" || receipt.Bytes != size {
				t.Fatalf("invalid upload result: calls=%d, receipt=%+v", calls, receipt)
			}
		})
	}
}

func TestMediaImageRejectsInvalidAcknowledgementsWithoutRetry(t *testing.T) {
	for name, mutate := range map[string]func(*wechat.UploadMsgImgResponse){
		"missing result": func(r *wechat.UploadMsgImgResponse) { r.BaseResponse = nil },
		"missing ret":    func(r *wechat.UploadMsgImgResponse) { r.BaseResponse.Ret = nil },
		"server failure": func(r *wechat.UploadMsgImgResponse) { r.BaseResponse.Ret = proto.Int32(-1) },
		"missing client": func(r *wechat.UploadMsgImgResponse) { r.ClientImgId = nil },
		"wrong client": func(r *wechat.UploadMsgImgResponse) {
			r.ClientImgId = &wechat.SKBuiltinString{Str: proto.String("other")}
		},
		"wrong recipient": func(r *wechat.UploadMsgImgResponse) {
			r.ToUserName = &wechat.SKBuiltinString{Str: proto.String("other")}
		},
		"wrong sender": func(r *wechat.UploadMsgImgResponse) {
			r.FromUserName = &wechat.SKBuiltinString{Str: proto.String("other")}
		},
		"missing total":  func(r *wechat.UploadMsgImgResponse) { r.TotalLen = nil },
		"wrong total":    func(r *wechat.UploadMsgImgResponse) { r.TotalLen = proto.Uint32(2) },
		"missing offset": func(r *wechat.UploadMsgImgResponse) { r.StartPos = nil },
		"stalled":        func(r *wechat.UploadMsgImgResponse) { r.StartPos, r.DataLen = proto.Uint32(0), proto.Uint32(0) },
		"partial":        func(r *wechat.UploadMsgImgResponse) { r.StartPos = proto.Uint32(2) },
		"beyond chunk":   func(r *wechat.UploadMsgImgResponse) { r.StartPos = proto.Uint32(6) },
		"wrong count":    func(r *wechat.UploadMsgImgResponse) { r.DataLen = proto.Uint32(4) },
		"no message ID":  func(r *wechat.UploadMsgImgResponse) { r.NewMsgId = nil },
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			exchange := func(_ string, _ uint32, request, response proto.Message) error {
				calls++
				ack := imageTestAck(request.(*wechat.UploadMsgImgRequest))
				ack.NewMsgId = proto.Int64(123)
				mutate(ack)
				*response.(*wechat.UploadMsgImgResponse) = *ack
				return nil
			}
			receipt, err := sendImageChunks(&wechat.BaseRequest{}, "sender", "receiver", []byte("image"), "client", time.Now(), exchange)
			if err == nil || calls != 1 || receipt == nil || receipt.ServerAccepted {
				t.Fatalf("invalid acknowledgement accepted or retried: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
			}
		})
	}
}

func TestMediaImageStopsAtFailedChunkAndAcceptsRangeAcknowledgement(t *testing.T) {
	for failAt := 1; failAt <= 2; failAt++ {
		calls := 0
		failure := errors.New("offline transport failure")
		exchange := func(_ string, _ uint32, request, response proto.Message) error {
			calls++
			if calls == failAt {
				return failure
			}
			*response.(*wechat.UploadMsgImgResponse) = *imageTestAck(request.(*wechat.UploadMsgImgRequest))
			return nil
		}
		receipt, err := sendImageChunks(&wechat.BaseRequest{}, "sender", "receiver", make([]byte, 100001), "client", time.Now(), exchange)
		if !errors.Is(err, failure) || calls != failAt || receipt.Chunks != failAt-1 || receipt.ServerAccepted {
			t.Fatalf("failed chunk was retried or skipped: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
		}
	}
	calls := 0
	exchange := func(_ string, _ uint32, request, response proto.Message) error {
		calls++
		req := request.(*wechat.UploadMsgImgRequest)
		ack := imageTestAck(req)
		ack.StartPos = req.StartPos
		ack.MsgId = proto.Uint32(11)
		if calls == 2 {
			ack.NewMsgId = proto.Int64(1<<60 + 17)
		}
		*response.(*wechat.UploadMsgImgResponse) = *ack
		return nil
	}
	receipt, err := sendImageChunks(&wechat.BaseRequest{}, "sender", "receiver", make([]byte, 50001), "client", time.Now(), exchange)
	if err != nil || !receipt.ServerAccepted || receipt.MessageID != "1152921504606846993" || calls != 2 {
		t.Fatalf("range acknowledgement or added 64-bit ID rejected: %+v, %v", receipt, err)
	}
}

func TestMediaImageRejectsChangedMessageIDsBeforeNextChunk(t *testing.T) {
	for _, useNewID := range []bool{false, true} {
		calls := 0
		exchange := func(_ string, _ uint32, request, response proto.Message) error {
			calls++
			ack := imageTestAck(request.(*wechat.UploadMsgImgRequest))
			if useNewID {
				ack.NewMsgId = proto.Int64(int64(calls))
			} else {
				ack.MsgId = proto.Uint32(uint32(calls))
			}
			*response.(*wechat.UploadMsgImgResponse) = *ack
			return nil
		}
		receipt, err := sendImageChunks(&wechat.BaseRequest{}, "sender", "receiver", make([]byte, 100001), "client", time.Now(), exchange)
		if err == nil || calls != 2 || receipt.Chunks != 1 || receipt.ServerAccepted {
			t.Fatalf("changed message ID accepted: calls=%d, receipt=%+v, err=%v", calls, receipt, err)
		}
	}
}
