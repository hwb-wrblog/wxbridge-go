package workflow

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image/gif"
	"io"
	"strconv"
	"strings"
	"time"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

// These bounds and the chunk size are local policy, not recovered server limits.
const MaxGIFBytes = 5 << 20
const MaxGIFFrames = 200
const gifMaxCanvasPixels = 4_000_000
const gifMaxFramePixels = 20_000_000
const gifChunkSize = 50_000

func validateGIFBytes(data []byte) error {
	if len(data) == 0 || len(data) > MaxGIFBytes {
		return fmt.Errorf("GIF must contain 1 to %d bytes", MaxGIFBytes)
	}
	frames, err := preflightGIF(data)
	if err != nil {
		return fmt.Errorf("invalid GIF: %w", err)
	}
	decoded, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid GIF image data: %w", err)
	}
	if len(decoded.Image) != frames {
		return errors.New("GIF decoded frame count differs from container")
	}
	return nil
}

// DecodeAll has no allocation budget. Inspect container bounds before allowing
// the standard decoder to allocate frames and validate compressed image data.
func preflightGIF(data []byte) (int, error) {
	r := bytes.NewReader(data)
	var header [13]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, err
	}
	if string(header[:6]) != "GIF87a" && string(header[:6]) != "GIF89a" {
		return 0, errors.New("expected GIF87a or GIF89a signature")
	}
	width := uint64(binary.LittleEndian.Uint16(header[6:8]))
	height := uint64(binary.LittleEndian.Uint16(header[8:10]))
	if width == 0 || height == 0 || width*height > gifMaxCanvasPixels {
		return 0, fmt.Errorf("GIF canvas exceeds %d pixels or has zero dimensions", gifMaxCanvasPixels)
	}
	if err := skipGIFColorTable(r, header[10]); err != nil {
		return 0, err
	}
	frames, pixels := 0, uint64(0)
	for {
		block, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		switch block {
		case 0x21:
			if _, err := r.ReadByte(); err != nil {
				return 0, err
			}
			if err := skipGIFSubBlocks(r); err != nil {
				return 0, err
			}
		case 0x2c:
			var descriptor [9]byte
			if _, err := io.ReadFull(r, descriptor[:]); err != nil {
				return 0, err
			}
			x := uint64(binary.LittleEndian.Uint16(descriptor[0:2]))
			y := uint64(binary.LittleEndian.Uint16(descriptor[2:4]))
			w := uint64(binary.LittleEndian.Uint16(descriptor[4:6]))
			h := uint64(binary.LittleEndian.Uint16(descriptor[6:8]))
			if w == 0 || h == 0 || x+w > width || y+h > height {
				return 0, errors.New("GIF frame is empty or outside the canvas")
			}
			frames++
			pixels += w * h
			if frames > MaxGIFFrames || pixels > gifMaxFramePixels {
				return 0, fmt.Errorf("GIF exceeds %d frames or %d cumulative frame pixels", MaxGIFFrames, gifMaxFramePixels)
			}
			if err := skipGIFColorTable(r, descriptor[8]); err != nil {
				return 0, err
			}
			if _, err := r.ReadByte(); err != nil {
				return 0, err
			}
			if err := skipGIFSubBlocks(r); err != nil {
				return 0, err
			}
		case 0x3b:
			if frames == 0 || r.Len() != 0 {
				return 0, errors.New("GIF has no frames or contains data after the trailer")
			}
			return frames, nil
		default:
			return 0, fmt.Errorf("unexpected GIF block 0x%02x", block)
		}
	}
}

func skipGIFBytes(r *bytes.Reader, size int) error {
	if size > r.Len() {
		return io.ErrUnexpectedEOF
	}
	_, err := r.Seek(int64(size), io.SeekCurrent)
	return err
}

func skipGIFColorTable(r *bytes.Reader, packed byte) error {
	if packed&0x80 == 0 {
		return nil
	}
	return skipGIFBytes(r, 3*(1<<((packed&7)+1)))
}

func skipGIFSubBlocks(r *bytes.Reader) error {
	for {
		size, err := r.ReadByte()
		if err != nil {
			return err
		}
		if size == 0 {
			return nil
		}
		if err := skipGIFBytes(r, int(size)); err != nil {
			return err
		}
	}
}

// SendGIF transmits original GIF bytes using the target's ForwardEmojiRequest
// envelope (0xb2ef40, CGI 175). A single-chunk custom GIF received a real
// server message ID through the HTTP API on 2026-10-23. Multipart uploads
// have offline coverage; recipient-side animation was not checked.
func (s *Session) SendGIF(to string, data []byte) (*MediaReceipt, error) {
	if err := validateGIFBytes(data); err != nil {
		return nil, err
	}
	if err := s.prepareMedia(to); err != nil {
		return nil, err
	}
	u := s.State.User
	if u.DeviceInfo == nil || u.WxId == "" {
		return nil, errors.New("GIF upload requires an authenticated iPad identity")
	}
	clientID, err := newMediaClientID()
	if err != nil {
		return nil, err
	}
	receipt, err := sendGIFChunks(clientsdk.GetBaseRequest(u), to, data, clientID, s.exchangeMedia)
	if err != nil {
		return receipt, err
	}
	return s.finishMedia(receipt)
}

func sendGIFChunks(base *wechat.BaseRequest, to string, data []byte, clientID string, exchange mediaExchange) (*MediaReceipt, error) {
	if base == nil || strings.TrimSpace(to) == "" || strings.TrimSpace(to) != to || clientID == "" || exchange == nil || len(data) == 0 || len(data) > MaxGIFBytes {
		return nil, errors.New("invalid GIF upload parameters")
	}
	// MD5 is the recovered protocol's content identifier, not authentication.
	sum := md5.Sum(data)
	digest := hex.EncodeToString(sum[:])
	receipt := &MediaReceipt{Kind: "gif", Recipient: to, ClientID: clientID, Bytes: len(data)}
	deadline := time.Now().Add(2 * time.Minute)
	var previousLegacyID uint32
	var previousNewID uint64
	for start := 0; start < len(data); start += gifChunkSize {
		if time.Now().After(deadline) {
			return receipt, errors.New("GIF upload deadline exceeded; do not retry blindly")
		}
		end := start + gifChunkSize
		if end > len(data) {
			end = len(data)
		}
		req := &wechat.UploadEmojiRequest{
			BaseRequest: base, EmojiItemCount: proto.Int32(1),
			EmojiItem: []*wechat.EmojiUploadInfoReq{{
				MD5: proto.String(digest), StartPos: proto.Int32(int32(start)), TotalLen: proto.Int32(int32(len(data))),
				EmojiBuffer: &wechat.BufferT{ILen: proto.Uint32(uint32(end - start)), Buffer: data[start:end]},
				Type:        proto.Int32(1), ToUserName: proto.String(to), ClientMsgID: proto.String(clientID),
			}},
		}
		var resp wechat.UploadEmojiResponse
		if err := exchange("/cgi-bin/micromsg-bin/sendemoji", 175, req, &resp); err != nil {
			return receipt, fmt.Errorf("GIF chunk at %d failed; delivery may be unknown, do not retry blindly: %w", start, err)
		}
		ack, err := validateGIFAcknowledgement(&resp, digest, len(data), end)
		if err != nil {
			return receipt, fmt.Errorf("GIF chunk at %d was not acknowledged; do not retry blindly: %w", start, err)
		}
		if legacyID := ack.GetMsgID(); legacyID != 0 {
			if previousLegacyID != 0 && previousLegacyID != legacyID {
				return receipt, errors.New("GIF response changed legacy message ID; do not retry blindly")
			}
			previousLegacyID = legacyID
		}
		if newID := ack.GetNewMsgId(); newID != 0 {
			if previousNewID != 0 && previousNewID != newID {
				return receipt, errors.New("GIF response changed server message ID; do not retry blindly")
			}
			previousNewID = newID
		}
		receipt.Chunks++
		if end == len(data) {
			receipt.UploadComplete = true
			if ack.GetMsgID() == 0 && ack.GetNewMsgId() == 0 {
				return receipt, errors.New("GIF bytes uploaded without a final server message ID; delivery is unknown, do not retry blindly")
			}
			id := previousNewID
			if id == 0 {
				id = uint64(previousLegacyID)
			}
			receipt.MessageID, receipt.ServerAccepted = strconv.FormatUint(id, 10), true
		}
	}
	return receipt, nil
}

func validateGIFAcknowledgement(resp *wechat.UploadEmojiResponse, digest string, total, end int) (*wechat.EmojiUploadInfoResp, error) {
	if err := responseOK(resp.GetBaseResponse()); err != nil {
		return nil, err
	}
	if resp.GetEmojiItemCount() != 1 || len(resp.GetEmojiItem()) != 1 || resp.EmojiItem[0] == nil {
		return nil, errors.New("GIF acknowledgement must contain exactly one item")
	}
	ack := resp.EmojiItem[0]
	if ack.Ret == nil || ack.GetRet() != 0 {
		return nil, fmt.Errorf("GIF item failed or omitted its result: %d", ack.GetRet())
	}
	if !strings.EqualFold(ack.GetMD5(), digest) || ack.TotalLen == nil || int64(ack.GetTotalLen()) != int64(total) {
		return nil, errors.New("GIF acknowledgement lacks matching MD5 and total length")
	}
	if ack.StartPos == nil || int64(ack.GetStartPos()) != int64(end) {
		return nil, errors.New("GIF acknowledgement is missing, partial, stalled, or outside the sent chunk")
	}
	if ack.MsgID == nil {
		return nil, errors.New("GIF acknowledgement omitted required message ID field")
	}
	return ack, nil
}
