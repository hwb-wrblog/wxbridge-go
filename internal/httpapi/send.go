package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"feiyu.com/wx/internal/workflow"
)

type sendRequest struct {
	To          string `json:"to"`
	Text        string `json:"text,omitempty"`
	UploadID    string `json:"upload_id,omitempty"`
	ThumbnailID string `json:"thumbnail_id,omitempty"`
	MD5         string `json:"md5,omitempty"`
	Size        int64  `json:"size,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	Seconds     int64  `json:"seconds,omitempty"`
}

type submission struct {
	RequestHash string          `json:"request_hash"`
	State       string          `json:"state"`
	Status      int             `json:"status,omitempty"`
	Response    json.RawMessage `json:"response,omitempty"`
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, workflow.MaxDownloadBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		respond(w, 413, map[string]interface{}{"ok": false, "error": "upload exceeds 32 MiB or could not be read"})
		return
	}
	if len(data) == 0 {
		respond(w, 400, map[string]interface{}{"ok": false, "error": "empty upload"})
		return
	}
	id := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := atomicWrite(filepath.Join(s.session.Dir, "uploads", id), data); err != nil {
		result(w, nil, err)
		return
	}
	respond(w, 201, map[string]interface{}{"ok": true, "upload_id": id, "bytes": len(data), "content_type": http.DetectContentType(data)})
}

func (s *Server) readUpload(id string, limit int) ([]byte, error) {
	if len(id) != 64 {
		return nil, errors.New("upload_id must be the SHA-256 returned by /v1/uploads")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return nil, errors.New("invalid upload_id")
	}
	path := filepath.Join(s.session.Dir, "uploads", strings.ToLower(id))
	st, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("upload_id does not exist")
	}
	if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > int64(limit) {
		return nil, fmt.Errorf("upload must contain 1 to %d bytes", limit)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit || fmt.Sprintf("%x", sha256.Sum256(data)) != strings.ToLower(id) {
		return nil, errors.New("stored upload has changed")
	}
	return data, nil
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, kind string) {
	switch kind {
	case "text", "image", "emoji", "gif", "voice", "video":
	default:
		respond(w, 404, map[string]interface{}{"ok": false, "error": "unknown message kind"})
		return
	}
	var body sendRequest
	if err := readJSON(w, r, &body); err != nil {
		respond(w, 400, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	if strings.TrimSpace(body.To) == "" || strings.TrimSpace(body.To) != body.To {
		respond(w, 400, map[string]interface{}{"ok": false, "error": "exact recipient username is required"})
		return
	}
	if kind == "text" && (strings.TrimSpace(body.Text) == "" || len(body.Text) > 65536) {
		respond(w, 400, map[string]interface{}{"ok": false, "error": "text must contain 1 to 65536 bytes"})
		return
	}
	if kind == "emoji" && (body.Size < 1 || body.Size > 2147483647) {
		respond(w, 400, map[string]interface{}{"ok": false, "error": "invalid sticker size"})
		return
	}
	if kind == "voice" && (body.DurationMS < 1 || body.DurationMS > 60000) {
		respond(w, 400, map[string]interface{}{"ok": false, "error": "duration_ms must be 1..60000"})
		return
	}
	if kind == "video" && (body.Seconds < 1 || body.Seconds > 600) {
		respond(w, 400, map[string]interface{}{"ok": false, "error": "seconds must be 1..600"})
		return
	}
	encoded, _ := json.Marshal(body)
	s.once(w, r, kind, encoded, func() (interface{}, error) {
		switch kind {
		case "text":
			response, err := s.session.Send(body.To, body.Text)
			if response == nil {
				return nil, err
			}
			receipt := &workflow.MediaReceipt{Kind: "text", Recipient: body.To, Bytes: len(body.Text)}
			if len(response.GetChatSendRetList()) == 1 && workflow.ValidateSend(response, body.To) == nil {
				ack := response.GetChatSendRetList()[0]
				id := uint64(ack.GetNewMsgId())
				if id == 0 {
					id = uint64(ack.GetMsgId())
				}
				receipt.MessageID, receipt.ServerAccepted, receipt.Chunks = strconv.FormatUint(id, 10), true, 1
			}
			return receipt, err
		case "emoji":
			return s.session.SendEmoji(body.To, body.MD5, int32(body.Size))
		case "image":
			data, err := s.readUpload(body.UploadID, workflow.MaxImageBytes)
			if err != nil {
				return nil, err
			}
			return s.session.SendImage(body.To, data)
		case "gif":
			data, err := s.readUpload(body.UploadID, workflow.MaxGIFBytes)
			if err != nil {
				return nil, err
			}
			return s.session.SendGIF(body.To, data)
		case "voice":
			data, err := s.readUpload(body.UploadID, workflow.MaxVoiceBytes)
			if err != nil {
				return nil, err
			}
			return s.session.SendVoice(body.To, data, int32(body.DurationMS))
		case "video":
			data, err := s.readUpload(body.UploadID, workflow.MaxVideoBytes)
			if err != nil {
				return nil, err
			}
			thumb, err := s.readUpload(body.ThumbnailID, workflow.MaxThumbnailBytes)
			if err != nil {
				return nil, err
			}
			return s.session.SendVideo(body.To, data, thumb, uint32(body.Seconds))
		}
		return nil, errors.New("unknown message kind")
	})
}

// The processing record is durable before the send. After a crash an unfinished
// request stays unknown rather than being silently sent for a second time.
func (s *Server) once(w http.ResponseWriter, r *http.Request, kind string, body []byte, operation func() (interface{}, error)) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
		respond(w, 400, map[string]interface{}{"ok": false, "error": "Idempotency-Key is required (1..128 characters)"})
		return
	}
	keyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	requestHash := fmt.Sprintf("%x", sha256.Sum256(append([]byte(kind+"\n"), body...)))
	path := filepath.Join(s.session.Dir, "submissions", keyHash+".json")
	data, err := os.ReadFile(path)
	if err == nil {
		var saved submission
		if err := json.Unmarshal(data, &saved); err != nil {
			result(w, nil, errors.New("saved submission is unreadable; do not retry with a new key"))
			return
		}
		if saved.RequestHash != requestHash {
			respond(w, 409, map[string]interface{}{"ok": false, "error": "Idempotency-Key was already used for another request"})
			return
		}
		if saved.State != "completed" {
			respond(w, 409, map[string]interface{}{"ok": false, "delivery_unknown": true, "error": "earlier submission was interrupted; inspect delivery before using a new key"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(saved.Status)
		w.Write(saved.Response)
		return
	}
	if !os.IsNotExist(err) {
		result(w, nil, err)
		return
	}
	saved := submission{RequestHash: requestHash, State: "processing"}
	data, _ = json.Marshal(saved)
	if err := atomicWrite(path, data); err != nil {
		result(w, nil, err)
		return
	}
	value, err := invoke(operation)
	response := map[string]interface{}{"ok": err == nil, "result": value}
	saved.Status = 200
	if err != nil {
		saved.Status = 502
		response["error"] = err.Error()
		response["retry_automatically"] = false
	}
	saved.State = "completed"
	saved.Response, _ = json.Marshal(response)
	data, _ = json.Marshal(saved)
	if err := atomicWrite(path, data); err != nil {
		respond(w, 500, map[string]interface{}{"ok": false, "delivery_unknown": true, "error": "submission result could not be persisted; do not retry with a new key"})
		return
	}
	respond(w, saved.Status, response)
}

func invoke(operation func() (interface{}, error)) (value interface{}, err error) {
	defer func() {
		if recover() != nil {
			value = nil
			err = errors.New("operation interrupted; delivery is unknown, do not retry automatically")
		}
	}()
	return operation()
}
