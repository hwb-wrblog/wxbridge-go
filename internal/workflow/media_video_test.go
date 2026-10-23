package workflow

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func TestCDNFramesRejectTruncationOversizeAndDuplicates(t *testing.T) {
	body := append(clientsdk.PackCdnRequestElementData("retcode", []byte("0")), clientsdk.PackCdnRequestElementData("fileid", []byte("test-id"))...)
	frame := make([]byte, 25)
	frame[0] = 0xab
	binary.BigEndian.PutUint32(frame[1:5], uint32(25+len(body)))
	binary.BigEndian.PutUint32(frame[21:25], uint32(len(body)))
	frame = append(frame, body...)
	fields, err := readCDNFrame(bytes.NewReader(frame))
	if err != nil || string(fields["fileid"]) != "test-id" || string(fields["retcode"]) != "0" {
		t.Fatalf("valid CDN frame rejected: %v", err)
	}
	for i := 0; i < len(frame); i++ {
		if _, err := readCDNFrame(bytes.NewReader(frame[:i])); err == nil {
			t.Fatalf("accepted frame truncated at %d", i)
		}
	}
	bad := append([]byte(nil), frame...)
	binary.BigEndian.PutUint32(bad[1:5], 1<<30)
	if _, err := readCDNFrame(bytes.NewReader(bad)); err == nil {
		t.Fatal("oversize frame accepted")
	}
	bad = append([]byte(nil), frame...)
	binary.BigEndian.PutUint32(bad[25:29], 0xffffffff)
	if _, err := readCDNFrame(bytes.NewReader(bad)); err == nil {
		t.Fatal("invalid field size accepted")
	}
	dup := clientsdk.PackCdnRequestElementData("retcode", []byte("-1"))
	bad = append(append([]byte(nil), frame...), dup...)
	binary.BigEndian.PutUint32(bad[1:5], uint32(len(bad)))
	binary.BigEndian.PutUint32(bad[21:25], uint32(len(bad)-25))
	if _, err := readCDNFrame(bytes.NewReader(bad)); err == nil {
		t.Fatal("duplicate result code accepted")
	}
}

func TestVideoUploadIsNotSendSuccess(t *testing.T) {
	upload := &uploadedVideo{fileID: "video-id", thumbnailID: "thumb-id", aesKey: "0123456789abcdef0123456789abcdef", md5: "test-md5", videoBytes: 123, thumbBytes: 45, width: 320, height: 180}
	for _, fault := range []string{"", "ret", "client", "id", "transport"} {
		t.Run(fault, func(t *testing.T) {
			receipt := &MediaReceipt{UploadComplete: true}
			calls := 0
			err := sendUploadedVideo(&wechat.BaseRequest{}, "from", "to", "client", upload, 2, func(path string, cgi uint32, request, response proto.Message) error {
				calls++
				req := request.(*wechat.UploadVideoRequest)
				if path != "/cgi-bin/micromsg-bin/uploadvideo" || cgi != 110 || req.GetCDNThumbImgWidth() != 320 || req.GetCDNThumbImgHeight() != 180 || req.GetVideoTotalLen() != 123 || req.GetAESKey() != upload.aesKey || req.GetToUserName() != "to" {
					t.Fatal("incorrect video metadata")
				}
				resp := wechat.UploadVideoResponse{BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(0)}, ClientMsgId: proto.String("client"), NewMsgId: proto.Uint64(9007199254740993)}
				switch fault {
				case "ret":
					resp.BaseResponse.Ret = proto.Int32(-1)
				case "client":
					resp.ClientMsgId = proto.String("another")
				case "id":
					resp.NewMsgId = nil
				case "transport":
					return errors.New("connection lost")
				}
				*response.(*wechat.UploadVideoResponse) = resp
				return nil
			}, receipt)
			if calls != 1 || (err == nil) != (fault == "") || receipt.ServerAccepted != (fault == "") {
				t.Fatalf("incorrect video result: %#v %v", receipt, err)
			}
			if fault == "" && receipt.MessageID != "9007199254740993" {
				t.Fatal("message ID lost precision")
			}
		})
	}
}
