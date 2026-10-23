package workflow

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"net"
	"net/url"
	"testing"

	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func TestDownloadXMLAndCDNURLPolicy(t *testing.T) {
	x, err := parseMediaXML("member:\n<msg><voicemsg length=\"108\" bufid=\"42\" voiceformat=\"4\"/></msg>")
	if err != nil || x.Voice.Length != 108 || x.Voice.Buffer != "42" {
		t.Fatal(x, err)
	}
	for _, raw := range []string{"file:///etc/passwd", "http://127.0.0.1/a", "http://qq.com.evil.test/a", "http://user:secret@emoji.qpic.cn/a", "https://emoji.qpic.cn:22/a"} {
		u, _ := url.Parse(raw)
		if validEmojiURL(u) == nil {
			t.Fatal("unsafe URL accepted", raw)
		}
	}
	u, _ := url.Parse("https://emoji.qpic.cn/test")
	if err := validEmojiURL(u); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"127.0.0.1", "10.2.3.4", "192.168.1.2", "169.254.169.254", "::1", "::ffff:127.0.0.1", "fd00::1"} {
		if publicMediaIP(net.ParseIP(value)) {
			t.Fatal("private IP accepted", value)
		}
	}
}

func TestMediaDecryptionChecksLengthAndPadding(t *testing.T) {
	key := []byte("0123456789abcdef")
	block, _ := aes.NewCipher(key)
	plain := append([]byte("known media"), bytes.Repeat([]byte{5}, 5)...)
	encoded := make([]byte, 16)
	block.Encrypt(encoded, plain)
	decoded, err := decryptMediaECB(encoded, hex.EncodeToString(key))
	if err != nil || string(decoded) != "known media" {
		t.Fatal(string(decoded), err)
	}
	plain[15] = 0
	block.Encrypt(encoded, plain)
	if _, err := decryptMediaECB(encoded, hex.EncodeToString(key)); err == nil {
		t.Fatal("bad padding accepted")
	}
	if _, err := decryptMediaECB(encoded[:15], hex.EncodeToString(key)); err == nil {
		t.Fatal("partial AES block accepted")
	}
}

func TestVoiceDownloadAccumulatesChunksAndRejectsMissingProgress(t *testing.T) {
	m := &wechat.AddMsg{NewMsgId: proto.Int64(9007199254740993), FromUserName: &wechat.SKBuiltinString{Str: proto.String("test")}}
	want := bytes.Repeat([]byte{7}, 50003)
	calls := 0
	data, err := downloadVoiceChunks(m, uint32(len(want)), 0, &wechat.BaseRequest{}, func(path string, cgi uint32, request, response proto.Message) error {
		calls++
		req := request.(*wechat.DownloadVoiceRequest)
		if path != "/cgi-bin/micromsg-bin/downloadvoice" || cgi != 128 {
			t.Fatal("wrong voice download endpoint")
		}
		chunk := want[req.GetOffset() : req.GetOffset()+req.GetLength()]
		*response.(*wechat.DownloadVoiceResponse) = wechat.DownloadVoiceResponse{BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(0)}, Offset: req.Offset, Length: req.Length, NewMsgId: req.NewMsgId, Data: &wechat.SKBuiltinString_{Len: req.Length, Buffer: chunk}}
		return nil
	})
	if err != nil || calls != 2 || !bytes.Equal(data, want) {
		t.Fatal("voice chunks were lost", calls, err)
	}
	_, err = downloadVoiceChunks(m, 2, 0, &wechat.BaseRequest{}, func(_ string, _ uint32, _ proto.Message, response proto.Message) error {
		*response.(*wechat.DownloadVoiceResponse) = wechat.DownloadVoiceResponse{BaseResponse: &wechat.BaseResponse{Ret: proto.Int32(0)}}
		return nil
	})
	if err == nil {
		t.Fatal("empty download reported success")
	}
}

func TestFailedDownloadIsRecordedWithoutPublishingAFile(t *testing.T) {
	s := &Session{Dir: t.TempDir(), State: State{Pending: []*wechat.AddMsg{{NewMsgId: proto.Int64(42), MsgType: proto.Uint32(3), FromUserName: &wechat.SKBuiltinString{Str: proto.String("test")}}}}}
	f, err := s.DownloadMedia("42")
	if err == nil || f.Status != "failed" || f.Filename != "" {
		t.Fatal(f, err)
	}
	if _, _, err := s.MediaPath("42"); err == nil {
		t.Fatal("failed download published")
	}
	if _, _, err := s.MediaPath("../../session.json"); err == nil {
		t.Fatal("path traversal accepted")
	}
}
