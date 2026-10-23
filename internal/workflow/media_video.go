package workflow

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/clientsdk/baseinfo"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

const MaxVideoBytes = 20 << 20
const MaxThumbnailBytes = 1 << 20

type uploadedVideo struct {
	fileID, thumbnailID, aesKey, md5 string
	videoBytes, thumbBytes           int
	width, height                    int
}

func validateVideo(video, thumbnail []byte, seconds uint32) (image.Config, error) {
	if len(video) == 0 || len(video) > MaxVideoBytes || http.DetectContentType(video) != "video/mp4" {
		return image.Config{}, errors.New("video requires an MP4 file no larger than 20 MiB")
	}
	if seconds == 0 || seconds > 600 {
		return image.Config{}, errors.New("video duration must be between 1 and 600 seconds")
	}
	if len(thumbnail) == 0 || len(thumbnail) > MaxThumbnailBytes {
		return image.Config{}, errors.New("video requires a JPEG thumbnail no larger than 1 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(thumbnail))
	if err != nil || format != "jpeg" || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 4_000_000 {
		return image.Config{}, errors.New("video thumbnail must be a valid JPEG of at most 4 million pixels")
	}
	if _, _, err := image.Decode(bytes.NewReader(thumbnail)); err != nil {
		return image.Config{}, fmt.Errorf("invalid video thumbnail: %w", err)
	}
	return config, nil
}

func (s *Session) SendVideo(to string, video, thumbnail []byte, seconds uint32) (*MediaReceipt, error) {
	config, err := validateVideo(video, thumbnail, seconds)
	if err != nil {
		return nil, err
	}
	if err := s.prepareMedia(to); err != nil {
		return nil, err
	}
	clientID, err := newMediaClientID()
	if err != nil {
		return nil, err
	}
	r := &MediaReceipt{Kind: "video", Recipient: to, ClientID: clientID, Bytes: len(video)}
	dns, err := s.mediaCDNDNS()
	if err != nil {
		return r, err
	}
	upload, err := uploadVideoCDN(s.State.User, dns, to, video, thumbnail, config)
	if err != nil {
		return r, fmt.Errorf("video CDN upload failed before message submission: %w", err)
	}
	r.UploadComplete = true
	if err := sendUploadedVideo(clientsdk.GetBaseRequest(s.State.User), s.State.User.WxId, to, clientID, upload, seconds, s.exchangeMedia, r); err != nil {
		return r, err
	}
	return s.finishMedia(r)
}

func (s *Session) mediaCDNDNS() (*wechat.CDNDnsInfo, error) {
	header, err := clientsdk.SendGetCDNDnsRequest(s.State.User)
	if err != nil {
		return nil, err
	}
	var response wechat.GetCDNDnsResponse
	if err := clientsdk.ParseResponseData(s.State.User, header, &response); err != nil {
		return nil, err
	}
	if err := responseOK(response.GetBaseResponse()); err != nil {
		return nil, err
	}
	dns := response.GetDnsInfo()
	if dns == nil || len(dns.GetAuthKey().GetBuffer()) == 0 || len(dns.GetFrontIplist()) == 0 || len(dns.GetFrontIpportList()) == 0 || len(dns.GetFrontIpportList()[0].GetPortList()) == 0 {
		return nil, errors.New("CDN DNS response has no usable authenticated endpoint")
	}
	s.State.User.DNSInfo, s.State.User.FAKEDnsInfo = dns, response.GetFakeDnsInfo()
	if err := s.Save(); err != nil {
		return nil, err
	}
	return dns, nil
}

func uploadVideoCDN(u *baseinfo.UserInfo, dns *wechat.CDNDnsInfo, to string, video, thumbnail []byte, config image.Config) (*uploadedVideo, error) {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	item := &baseinfo.UploadVideoItem{ToUser: to, VideoData: video, ThumbData: thumbnail, CDNDns: dns, AesKey: key, Seq: 1}
	req, err := clientsdk.CreateVideoUploadRequest(u, item)
	if err != nil {
		return nil, err
	}
	// The reference helper contains a sample filename and MP4 identity.
	// Populate identity from this payload; encryption uses a fresh key per file.
	req.LocalName, req.Mp4identify = req.RawFileMd5+".mp4", req.RawFileMd5
	if len(req.ClientRsaVal) == 0 || len(req.FileData) == 0 || len(req.ThumbData) == 0 {
		return nil, errors.New("CDN request encryption failed")
	}
	host, port := dns.GetFrontIplist()[0].GetStr(), dns.GetFrontIpportList()[0].GetPortList()[0]
	if net.ParseIP(host) == nil || port == 0 || port > 65535 {
		return nil, errors.New("CDN returned an invalid IP address or port")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(45 * time.Second)); err != nil {
		return nil, err
	}
	if _, err := io.Copy(conn, bytes.NewReader(clientsdk.PackCdnVideoUploadRequest(req))); err != nil {
		return nil, err
	}
	for packets := 0; packets < 64; packets++ {
		fields, err := readCDNFrame(conn)
		if err != nil {
			return nil, err
		}
		ret, ok := fields["retcode"]
		if !ok {
			return nil, errors.New("CDN response lacks a result code")
		}
		code, err := strconv.ParseInt(string(ret), 10, 64)
		if err != nil || code != 0 {
			return nil, fmt.Errorf("CDN rejected upload (retcode %q)", string(ret))
		}
		// The server may replace filekey. This connection carries one upload;
		// bind completion to its sequence, result code and received size instead.
		if seq, ok := fields["seq"]; ok && string(seq) != strconv.Itoa(int(req.Seq)) {
			return nil, errors.New("CDN response sequence mismatch")
		}
		if fileID := string(fields["fileid"]); fileID != "" {
			if recv, ok := fields["recvlen"]; ok {
				n, err := strconv.ParseUint(string(recv), 10, 32)
				if err != nil || n != uint64(req.TotalSize) {
					return nil, errors.New("CDN completed with inconsistent received length")
				}
			}
			thumbID := string(fields["thumburl"])
			if thumbID == "" {
				thumbID = fileID
			}
			return &uploadedVideo{fileID: fileID, thumbnailID: thumbID, aesKey: hex.EncodeToString(key), md5: req.RawFileMd5,
				videoBytes: len(video), thumbBytes: len(thumbnail), width: config.Width, height: config.Height}, nil
		}
	}
	return nil, errors.New("CDN did not finish within 64 responses")
}

// CDN uses a 25-byte header followed by big-endian length-prefixed key/value
// pairs. Check framing before decoding so a short read cannot become a slice panic.
func readCDNFrame(r io.Reader) (map[string][]byte, error) {
	return readCDNFrameLimit(r, 1<<20)
}

func readCDNFrameLimit(r io.Reader, limit uint32) (map[string][]byte, error) {
	header := make([]byte, 25)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	total, bodyLen := binary.BigEndian.Uint32(header[1:5]), binary.BigEndian.Uint32(header[21:25])
	if header[0] != 0xab || total < 25 || total > limit || bodyLen != total-25 {
		return nil, errors.New("invalid CDN frame header or length")
	}
	body := make([]byte, int(bodyLen))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	fields := make(map[string][]byte)
	for len(body) != 0 {
		if len(body) < 4 {
			return nil, errors.New("truncated CDN field name length")
		}
		n := binary.BigEndian.Uint32(body)
		body = body[4:]
		if n == 0 || n > 256 || uint64(n)+4 > uint64(len(body)) {
			return nil, errors.New("invalid CDN field name length")
		}
		name := string(body[:n])
		body = body[n:]
		n = binary.BigEndian.Uint32(body)
		body = body[4:]
		if uint64(n) > uint64(len(body)) {
			return nil, errors.New("truncated CDN field value")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("duplicate CDN response field")
		}
		fields[name], body = body[:n], body[n:]
		if len(fields) > 256 {
			return nil, errors.New("too many CDN fields")
		}
	}
	return fields, nil
}

func sendUploadedVideo(base *wechat.BaseRequest, from, to, clientID string, upload *uploadedVideo, seconds uint32, exchange mediaExchange, receipt *MediaReceipt) error {
	if upload == nil || upload.fileID == "" || len(upload.aesKey) != 32 || base == nil || exchange == nil {
		return errors.New("video upload metadata is incomplete")
	}
	req := &wechat.UploadVideoRequest{
		BaseRequest: base, ClientMsgId: proto.String(clientID), FromUserName: proto.String(from), ToUserName: proto.String(to),
		ThumbTotalLen: proto.Uint32(uint32(upload.thumbBytes)), ThumbStartPos: proto.Uint32(0),
		ThumbData:     &wechat.BufferT{ILen: proto.Uint32(0), Buffer: []byte{}},
		VideoTotalLen: proto.Uint32(uint32(upload.videoBytes)), VideoStartPos: proto.Uint32(0),
		VideoData:  &wechat.BufferT{ILen: proto.Uint32(0), Buffer: []byte{}},
		PlayLength: proto.Uint32(seconds), NetworkEnv: proto.Uint32(1), CameraType: proto.Uint32(2),
		FuncFlag: proto.Uint32(0), MsgSource: proto.String(""),
		CDNVideoUrl: proto.String(upload.fileID), CDNThumbUrl: proto.String(upload.thumbnailID),
		AESKey: proto.String(upload.aesKey), CDNThumbAESKey: proto.String(upload.aesKey), EncryVer: proto.Int32(1),
		CDNThumbImgSize: proto.Int32(int32(upload.thumbBytes)), CDNThumbImgHeight: proto.Int32(int32(upload.height)), CDNThumbImgWidth: proto.Int32(int32(upload.width)),
		VideoFrom: proto.Int32(0), ReqTime: proto.Uint32(uint32(time.Now().Unix())),
		VideoMd5: proto.String(upload.md5), MsgForwardType: proto.Uint32(1),
	}
	var response wechat.UploadVideoResponse
	if err := exchange("/cgi-bin/micromsg-bin/uploadvideo", 110, req, &response); err != nil {
		return fmt.Errorf("video uploaded but message acknowledgement unavailable: %w", err)
	}
	if err := responseOK(response.GetBaseResponse()); err != nil {
		return err
	}
	if response.GetClientMsgId() != clientID {
		return errors.New("video acknowledgement client ID mismatch")
	}
	if response.GetNewMsgId() != 0 {
		receipt.MessageID = strconv.FormatUint(response.GetNewMsgId(), 10)
	} else if response.GetMsgId() != 0 {
		receipt.MessageID = strconv.FormatUint(uint64(response.GetMsgId()), 10)
	} else {
		return errors.New("video acknowledgement has no server message ID; do not retry blindly")
	}
	receipt.Chunks, receipt.ServerAccepted = 1, true
	return nil
}
