package workflow

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

const MaxDownloadBytes = 32 << 20

type MediaFile struct {
	MessageID   string `json:"message_id"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Error       string `json:"error,omitempty"`
	Attempts    int    `json:"attempts"`
	UpdatedAt   int64  `json:"updated_at"`
}

func MessageID(m *wechat.AddMsg) string {
	if m.GetNewMsgId() != 0 {
		return strconv.FormatUint(uint64(m.GetNewMsgId()), 10)
	}
	return strconv.FormatUint(uint64(m.GetMsgId()), 10)
}

func MediaKind(t uint32) string {
	switch t {
	case 3:
		return "image"
	case 34:
		return "voice"
	case 43, 62:
		return "video"
	case 47:
		return "emoji"
	}
	return ""
}

func (s *Session) MediaFiles() (map[string]*MediaFile, error) {
	files := make(map[string]*MediaFile)
	data, err := os.ReadFile(filepath.Join(s.Dir, "media-index.json"))
	if os.IsNotExist(err) {
		return files, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, err
	}
	if files == nil {
		files = make(map[string]*MediaFile)
	}
	return files, nil
}

func (s *Session) MediaPath(id string) (string, *MediaFile, error) {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil || id == "0" {
		return "", nil, errors.New("invalid media message ID")
	}
	files, err := s.MediaFiles()
	if err != nil {
		return "", nil, err
	}
	f := files[id]
	if f == nil || f.Status != "ready" || filepath.Base(f.Filename) != f.Filename || f.Filename == "." || f.Filename == "" {
		return "", f, errors.New("media is not ready")
	}
	path := filepath.Join(s.Dir, "media", f.Filename)
	st, err := os.Lstat(path)
	if err != nil {
		return "", f, err
	}
	if !st.Mode().IsRegular() || st.Size() != int64(f.Bytes) {
		return "", f, errors.New("media file is missing or has changed size")
	}
	return path, f, nil
}

func (s *Session) DownloadMedia(id string) (*MediaFile, error) {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil || id == "0" {
		return nil, errors.New("invalid media message ID")
	}
	if _, f, err := s.MediaPath(id); err == nil {
		return f, nil
	}
	var message *wechat.AddMsg
	for i := len(s.State.Pending) - 1; i >= 0; i-- {
		if MessageID(s.State.Pending[i]) == id {
			message = s.State.Pending[i]
			break
		}
	}
	if message == nil {
		return nil, errors.New("message is not in the saved inbox")
	}
	kind := MediaKind(message.GetMsgType())
	if kind == "" {
		return nil, errors.New("message is not supported media")
	}
	files, err := s.MediaFiles()
	if err != nil {
		return nil, err
	}
	attempts := 1
	if previous := files[id]; previous != nil {
		attempts += previous.Attempts
	}
	f := &MediaFile{MessageID: id, Kind: kind, Status: "failed", Attempts: attempts, UpdatedAt: time.Now().Unix()}
	data, err := s.downloadMessageBytes(message)
	if err == nil {
		var extension string
		f.ContentType, extension, err = downloadedType(kind, data)
		if err == nil {
			dir := filepath.Join(s.Dir, "media")
			if err = os.MkdirAll(dir, 0700); err == nil {
				f.Filename = id + extension
				err = writePrivateFile(filepath.Join(dir, f.Filename), data)
			}
			if err == nil {
				f.Status, f.Bytes, f.SHA256 = "ready", len(data), fmt.Sprintf("%x", sha256.Sum256(data))
			}
		}
	}
	if err != nil {
		f.Error = err.Error()
	}
	files[id] = f
	index, marshalErr := json.Marshal(files)
	if marshalErr != nil {
		return f, marshalErr
	}
	if saveErr := writePrivateFile(filepath.Join(s.Dir, "media-index.json"), index); saveErr != nil {
		return f, saveErr
	}
	return f, err
}

func writePrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

type mediaXML struct {
	Image struct {
		AES string `xml:"aeskey,attr"`
		Big string `xml:"cdnbigimgurl,attr"`
		Mid string `xml:"cdnmidimgurl,attr"`
		MD5 string `xml:"md5,attr"`
	} `xml:"img"`
	Video struct {
		AES string `xml:"aeskey,attr"`
		URL string `xml:"cdnvideourl,attr"`
		MD5 string `xml:"md5,attr"`
	} `xml:"videomsg"`
	Voice struct {
		Length uint32 `xml:"length,attr"`
		Buffer string `xml:"bufid,attr"`
		Format uint32 `xml:"voiceformat,attr"`
	} `xml:"voicemsg"`
	Emoji struct {
		URL          string `xml:"cdnurl,attr"`
		EncryptedURL string `xml:"encrypturl,attr"`
		AES          string `xml:"aeskey,attr"`
		MD5          string `xml:"md5,attr"`
	} `xml:"emoji"`
}

func parseMediaXML(content string) (*mediaXML, error) {
	if len(content) > 1<<20 {
		return nil, errors.New("media XML is too large")
	}
	start := strings.Index(content, "<")
	if start < 0 {
		return nil, errors.New("media message has no XML")
	}
	var message mediaXML
	if err := xml.Unmarshal([]byte(content[start:]), &message); err != nil {
		return nil, fmt.Errorf("invalid media XML: %w", err)
	}
	return &message, nil
}

func (s *Session) downloadMessageBytes(m *wechat.AddMsg) ([]byte, error) {
	if err := s.prepareMedia(m.GetFromUserName().GetStr()); err != nil {
		return nil, err
	}
	x, err := parseMediaXML(m.GetContent().GetStr())
	if err != nil {
		return nil, err
	}
	var data []byte
	var expectedMD5 string
	switch MediaKind(m.GetMsgType()) {
	case "image":
		fileID, fileType := x.Image.Big, uint32(1)
		if fileID == "" {
			fileID, fileType = x.Image.Mid, 2
		} else {
			expectedMD5 = x.Image.MD5
		}
		data, err = s.downloadCDN(fileID, x.Image.AES, fileType)
	case "video":
		expectedMD5 = x.Video.MD5
		data, err = s.downloadCDN(x.Video.URL, x.Video.AES, 4)
	case "voice":
		data, err = s.downloadVoice(m, x)
	case "emoji":
		expectedMD5 = x.Emoji.MD5
		if x.Emoji.URL != "" {
			data, err = downloadEmojiURL(x.Emoji.URL)
		} else {
			data, err = downloadEmojiURL(x.Emoji.EncryptedURL)
			if err == nil {
				data, err = decryptMediaECB(data, x.Emoji.AES)
			}
		}
	default:
		return nil, errors.New("unsupported media type")
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > MaxDownloadBytes {
		return nil, errors.New("download is empty or too large")
	}
	if expectedMD5 != "" && !strings.EqualFold(fmt.Sprintf("%x", md5.Sum(data)), expectedMD5) {
		return nil, errors.New("downloaded media MD5 does not match the message")
	}
	return data, nil
}

func downloadedType(kind string, data []byte) (string, string, error) {
	if kind == "voice" {
		format, err := VoiceFormat(data)
		if err != nil {
			return "", "", err
		}
		if format == 0 {
			return "audio/amr", ".amr", nil
		}
		return "audio/silk", ".silk", nil
	}
	if kind == "video" {
		if http.DetectContentType(data) != "video/mp4" {
			return "", "", errors.New("downloaded video is not MP4")
		}
		return "video/mp4", ".mp4", nil
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 40_000_000 {
		return "", "", errors.New("downloaded image is invalid or too large")
	}
	switch format {
	case "png":
		return "image/png", ".png", nil
	case "jpeg":
		return "image/jpeg", ".jpg", nil
	case "gif":
		return "image/gif", ".gif", nil
	}
	return "", "", errors.New("unsupported downloaded image format")
}

func (s *Session) downloadCDN(fileID, aesKey string, fileType uint32) ([]byte, error) {
	key, err := hex.DecodeString(aesKey)
	if err != nil || len(key) != 16 || fileID == "" {
		return nil, errors.New("media message lacks a CDN file ID or 16-byte key")
	}
	dns, err := s.mediaCDNDNS()
	if err != nil {
		return nil, err
	}
	req := clientsdk.CreateCDNDownloadRequest(s.State.User, dns, key, fileID, fileType)
	host, port := dns.GetFrontIplist()[0].GetStr(), dns.GetFrontIpportList()[0].GetPortList()[0]
	if net.ParseIP(host) == nil || port == 0 || port > 65535 {
		return nil, errors.New("invalid CDN endpoint")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(45 * time.Second)); err != nil {
		return nil, err
	}
	if _, err := io.Copy(conn, bytes.NewReader(clientsdk.PackCdnImageDownloadRequest(req))); err != nil {
		return nil, err
	}
	var encoded []byte
	var total uint64
	for packets := 0; packets < 512; packets++ {
		fields, err := readCDNFrameLimit(conn, MaxDownloadBytes+65536)
		if err != nil {
			return nil, err
		}
		if string(fields["retcode"]) != "0" {
			return nil, fmt.Errorf("CDN download rejected with result %q", string(fields["retcode"]))
		}
		n, err := strconv.ParseUint(string(fields["totalsize"]), 10, 32)
		if err != nil || n == 0 || n > MaxDownloadBytes+16 || (total != 0 && total != n) {
			return nil, errors.New("invalid CDN download total length")
		}
		total = n
		if start, ok := fields["rangestart"]; ok {
			offset, err := strconv.ParseUint(string(start), 10, 32)
			if err != nil || offset != uint64(len(encoded)) {
				return nil, errors.New("CDN download returned noncontiguous data")
			}
		}
		chunk := fields["filedata"]
		if len(chunk) == 0 || uint64(len(encoded))+uint64(len(chunk)) > total {
			return nil, errors.New("CDN returned empty or oversized file data")
		}
		encoded = append(encoded, chunk...)
		if uint64(len(encoded)) == total {
			return decryptMediaECB(encoded, aesKey)
		}
	}
	return nil, errors.New("CDN download did not finish within 512 frames")
}

func decryptMediaECB(data []byte, keyHex string) ([]byte, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 16 || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("invalid encrypted media length or key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	decoded := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(decoded[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	padding := int(decoded[len(decoded)-1])
	if padding < 1 || padding > aes.BlockSize {
		return nil, errors.New("invalid media padding")
	}
	for _, b := range decoded[len(decoded)-padding:] {
		if int(b) != padding {
			return nil, errors.New("invalid media padding")
		}
	}
	return decoded[:len(decoded)-padding], nil
}

func (s *Session) downloadVoice(m *wechat.AddMsg, x *mediaXML) ([]byte, error) {
	if x.Voice.Length == 0 || x.Voice.Length > MaxVoiceBytes {
		return nil, errors.New("voice metadata lacks a valid byte length")
	}
	bufID := uint64(0)
	if x.Voice.Buffer != "" {
		var err error
		bufID, err = strconv.ParseUint(x.Voice.Buffer, 10, 64)
		if err != nil {
			return nil, errors.New("invalid voice buffer ID")
		}
	}
	return downloadVoiceChunks(m, x.Voice.Length, bufID, clientsdk.GetBaseRequest(s.State.User), s.exchangeMedia)
}

func downloadVoiceChunks(m *wechat.AddMsg, total uint32, bufferID uint64, base *wechat.BaseRequest, exchange mediaExchange) ([]byte, error) {
	if m == nil || total == 0 || total > MaxVoiceBytes || exchange == nil {
		return nil, errors.New("invalid voice download parameters")
	}
	data := make([]byte, 0, int(total))
	deadline := time.Now().Add(2 * time.Minute)
	for start := uint32(0); start < total; {
		if time.Now().After(deadline) {
			return nil, errors.New("voice download deadline exceeded")
		}
		length := total - start
		if length > 50000 {
			length = 50000
		}
		req := &wechat.DownloadVoiceRequest{BaseRequest: base, MsgId: proto.Uint32(m.GetMsgId()), Offset: proto.Uint32(start), Length: proto.Uint32(length), NewMsgId: proto.Uint64(uint64(m.GetNewMsgId())), ChatRoomName: proto.String(m.GetFromUserName().GetStr()), MasterBufId: proto.Uint64(bufferID)}
		var response wechat.DownloadVoiceResponse
		if err := exchange("/cgi-bin/micromsg-bin/downloadvoice", 128, req, &response); err != nil {
			return nil, err
		}
		if err := responseOK(response.GetBaseResponse()); err != nil {
			return nil, err
		}
		if response.GetCancelFlag() != 0 {
			return nil, errors.New("voice download cancelled")
		}
		chunk := response.GetData().GetBuffer()
		if len(chunk) == 0 || uint32(len(chunk)) > length || response.GetLength() != uint32(len(chunk)) || response.GetData().GetLen() != uint32(len(chunk)) {
			return nil, errors.New("voice download chunk length mismatch")
		}
		if response.Offset == nil || (response.GetOffset() != start && response.GetOffset() != start+uint32(len(chunk))) {
			return nil, errors.New("voice download offset mismatch")
		}
		if response.NewMsgId != nil && response.GetNewMsgId() != uint64(m.GetNewMsgId()) {
			return nil, errors.New("voice download message ID mismatch")
		}
		data = append(data, chunk...)
		start += uint32(len(chunk))
		if response.GetEndFlag() != 0 && start != total {
			return nil, errors.New("voice download ended before all bytes arrived")
		}
	}
	return data, nil
}

func validEmojiURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Port() != "" && u.Port() != "80" && u.Port() != "443") {
		return errors.New("invalid emoji download URL")
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range []string{".qq.com", ".qpic.cn", ".wechat.com", ".weixin.com"} {
		if strings.HasSuffix(host, suffix) {
			return nil
		}
	}
	return errors.New("emoji download host is not a supported WeChat CDN")
}

func downloadEmojiURL(rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err := validEmojiURL(u); err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !publicMediaIP(ip.IP) {
				continue
			}
			conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, errors.New("no reachable public emoji CDN address")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many emoji redirects")
		}
		return validEmojiURL(req.URL)
	}}
	response, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("emoji CDN returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > MaxDownloadBytes {
		return nil, errors.New("emoji download is empty or too large")
	}
	return data, nil
}

func publicMediaIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "0.0.0.0/8", "169.254.0.0/16", "fc00::/7"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}
	return true
}
