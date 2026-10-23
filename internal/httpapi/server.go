package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"feiyu.com/wx/internal/workflow"
)

type Config struct {
	Listen       string
	Token        string
	SyncInterval time.Duration
	AutoDownload bool
}

type Server struct {
	mu            sync.Mutex
	session       *workflow.Session
	config        Config
	tokenHash     [32]byte
	lastSync      int64
	lastHeartbeat int64
	lastError     string
	autoSince     int64
}

func New(session *workflow.Session, config Config) (*Server, error) {
	if session == nil || len(config.Token) < 32 {
		return nil, errors.New("session and an API token of at least 32 characters are required")
	}
	if config.Listen == "" {
		config.Listen = "127.0.0.1:8787"
	}
	if config.SyncInterval < time.Second {
		config.SyncInterval = 5 * time.Second
	}
	for _, sub := range []string{"uploads", "submissions"} {
		if err := os.MkdirAll(filepath.Join(session.Dir, sub), 0700); err != nil {
			return nil, err
		}
	}
	s := &Server{session: session, config: config, tokenHash: sha256.Sum256([]byte(config.Token)), autoSince: time.Now().Unix()}
	path := filepath.Join(session.Dir, "api-state.json")
	data, err := os.ReadFile(path)
	var saved struct {
		AutoSince int64 `json:"auto_since"`
	}
	if err == nil {
		if err := json.Unmarshal(data, &saved); err != nil {
			return nil, err
		}
		if saved.AutoSince > 0 {
			s.autoSince = saved.AutoSince
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	saved.AutoSince = s.autoSince
	data, _ = json.Marshal(saved)
	if err := atomicWrite(path, data); err != nil {
		return nil, err
	}
	return s, nil
}

func TokenFile(dir string) (string, error) {
	path := filepath.Join(dir, "api-token")
	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) < 32 {
			return "", errors.New("api-token must contain at least 32 characters")
		}
		if err := os.Chmod(path, 0600); err != nil {
			return "", err
		}
		return token, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random[:])
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	return token, f.Close()
}

func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.config.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.background(workerCtx)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
		}
		return nil
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	provided := r.Header.Get("Authorization")
	if !strings.HasPrefix(provided, "Bearer ") {
		respond(w, 401, map[string]interface{}{"ok": false, "error": "Bearer token required"})
		return
	}
	hash := sha256.Sum256([]byte(strings.TrimPrefix(provided, "Bearer ")))
	if subtle.ConstantTimeCompare(hash[:], s.tokenHash[:]) != 1 {
		respond(w, 401, map[string]interface{}{"ok": false, "error": "invalid API token"})
		return
	}
	defer func() {
		if recover() != nil {
			respond(w, 500, map[string]interface{}{"ok": false, "error": "operation failed; check local logs and delivery status before retrying"})
		}
	}()
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "/v1/uploads" && r.Method == http.MethodPost {
		s.upload(w, r)
		return
	}
	if strings.HasPrefix(path, "/v1/media/") && r.Method == http.MethodGet {
		s.mediaFile(w, r, strings.TrimPrefix(path, "/v1/media/"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.Context().Err(); err != nil {
		return
	}
	switch {
	case path == "/healthz" && r.Method == http.MethodGet:
		respond(w, 200, map[string]interface{}{"ok": true, "service": "wxbridge"})
	case path == "/v1/status" && r.Method == http.MethodGet:
		respond(w, 200, map[string]interface{}{"ok": true, "authenticated": s.session.State.Authenticated, "client_version": fmt.Sprintf("%#x", workflow.ClientVersion), "pending_messages": len(s.session.State.Pending), "last_sync": s.lastSync, "last_heartbeat": s.lastHeartbeat, "last_error": s.lastError, "auto_download": s.config.AutoDownload})
	case path == "/v1/capabilities" && r.Method == http.MethodGet:
		respond(w, 200, map[string]interface{}{"ok": true, "live_verified": []string{"qr_login", "text", "image", "existing_emoji", "custom_gif_server_ack", "amr_voice", "mp4_video"}, "offline_only": []string{"automatic_media_download", "silk_voice"}, "realtime_calls": false})
	case path == "/v1/messages" && r.Method == http.MethodGet:
		s.messages(w, r)
	case path == "/v1/login/qr" && r.Method == http.MethodPost:
		_, err := s.session.QR()
		result(w, map[string]interface{}{"image_url": "/v1/login/qr/image", "expires": s.session.State.Expires}, err)
	case path == "/v1/login/qr/image" && r.Method == http.MethodGet:
		for _, ext := range []string{"jpeg", "png"} {
			path := filepath.Join(s.session.Dir, "qrcode."+ext)
			if _, err := os.Stat(path); err == nil {
				http.ServeFile(w, r, path)
				return
			}
		}
		respond(w, 404, map[string]interface{}{"ok": false, "error": "QR image unavailable"})
	case path == "/v1/login/check" && r.Method == http.MethodPost:
		state, err := s.session.Check()
		if err == nil && state == 2 {
			err = s.session.Login()
		}
		result(w, map[string]interface{}{"qr_state": state, "authenticated": s.session.State.Authenticated}, err)
	case path == "/v1/heartbeat" && r.Method == http.MethodPost:
		err := s.session.Heartbeat()
		if err == nil {
			s.lastHeartbeat = time.Now().Unix()
		}
		result(w, map[string]bool{"online": err == nil}, err)
	case path == "/v1/sync" && r.Method == http.MethodPost:
		messages, more, err := s.session.Sync()
		if err == nil {
			s.lastSync = time.Now().Unix()
		}
		result(w, map[string]interface{}{"received": len(messages), "more": more}, err)
	case strings.HasPrefix(path, "/v1/messages/") && r.Method == http.MethodPost:
		s.send(w, r, strings.TrimPrefix(path, "/v1/messages/"))
	case strings.HasPrefix(path, "/v1/media/") && strings.HasSuffix(path, "/download") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/media/"), "/download")
		file, err := s.session.DownloadMedia(id)
		result(w, file, err)
	case path == "/v1/contacts/lookup" && r.Method == http.MethodPost:
		var body struct {
			Username string `json:"username"`
		}
		if err := readJSON(w, r, &body); err != nil {
			respond(w, 400, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		contact, err := s.session.Contact(body.Username)
		result(w, contact, err)
	default:
		respond(w, 404, map[string]interface{}{"ok": false, "error": "unknown endpoint or method"})
	}
}

func respond(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func result(w http.ResponseWriter, value interface{}, err error) {
	body := map[string]interface{}{"ok": err == nil, "result": value}
	status := 200
	if err != nil {
		body["error"], status = err.Error(), 502
	}
	respond(w, status, body)
}

func readJSON(w http.ResponseWriter, r *http.Request, out interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return errors.New("exactly one JSON object is required")
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
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
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	cursor, err := strconv.Atoi(r.URL.Query().Get("cursor"))
	if r.URL.Query().Get("cursor") == "" {
		cursor, err = 0, nil
	}
	if err != nil || cursor < 0 || cursor > len(s.session.State.Pending) {
		respond(w, 400, map[string]interface{}{"ok": false, "error": "invalid message cursor"})
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			respond(w, 400, map[string]interface{}{"ok": false, "error": "limit must be 1..100"})
			return
		}
	}
	files, err := s.session.MediaFiles()
	if err != nil {
		result(w, nil, err)
		return
	}
	peer := r.URL.Query().Get("peer")
	items := make([]map[string]interface{}, 0)
	for cursor < len(s.session.State.Pending) && len(items) < limit {
		message := s.session.State.Pending[cursor]
		cursor++
		from, to := message.GetFromUserName().GetStr(), message.GetToUserName().GetStr()
		if peer != "" && from != peer && to != peer {
			continue
		}
		id := workflow.MessageID(message)
		item := map[string]interface{}{"id": id, "from": from, "to": to, "type": message.GetMsgType(), "created": message.GetCreateTime()}
		if message.GetMsgType() == 1 {
			item["text"] = message.GetContent().GetStr()
		}
		if kind := workflow.MediaKind(message.GetMsgType()); kind != "" {
			item["media_kind"] = kind
			if file := files[id]; file != nil {
				item["media"] = file
				if file.Status == "ready" {
					item["download_url"] = "/v1/media/" + id
				}
			}
		}
		items = append(items, item)
	}
	respond(w, 200, map[string]interface{}{"ok": true, "messages": items, "next_cursor": cursor, "more": cursor < len(s.session.State.Pending)})
}

func (s *Server) mediaFile(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	path, file, err := s.session.MediaPath(id)
	s.mu.Unlock()
	if err != nil {
		respond(w, 404, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", file.Filename))
	http.ServeFile(w, r, path)
}

func (s *Server) background(ctx context.Context) {
	ticker := time.NewTicker(s.config.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		s.backgroundOnce(ctx)
		s.mu.Unlock()
	}
}

func (s *Server) backgroundOnce(ctx context.Context) {
	defer func() {
		if recover() != nil {
			s.lastError = "background operation failed"
		}
	}()
	if ctx.Err() != nil || !s.session.State.Authenticated {
		return
	}
	_, _, err := s.session.Sync()
	if err != nil {
		s.lastError = err.Error()
		if s.session.State.User != nil {
			s.session.State.User.MMInfo = nil
		}
		return
	}
	s.lastSync, s.lastError = time.Now().Unix(), ""
	if time.Now().Unix()-s.lastHeartbeat >= 60 {
		if err := s.session.Heartbeat(); err != nil {
			s.lastError = err.Error()
		} else {
			s.lastHeartbeat = time.Now().Unix()
		}
	}
	if !s.config.AutoDownload || ctx.Err() != nil {
		return
	}
	files, err := s.session.MediaFiles()
	if err != nil {
		s.lastError = err.Error()
		return
	}
	for _, message := range s.session.State.Pending {
		if int64(message.GetCreateTime()) < s.autoSince || workflow.MediaKind(message.GetMsgType()) == "" || message.GetFromUserName().GetStr() == s.session.State.User.WxId {
			continue
		}
		id := workflow.MessageID(message)
		if id == "0" {
			continue
		}
		if f := files[id]; f != nil && (f.Status == "ready" || f.Attempts >= 3 || time.Now().Unix()-f.UpdatedAt < 60) {
			continue
		}
		if _, err := s.session.DownloadMedia(id); err != nil {
			s.lastError = "media download: " + err.Error()
		}
		break
	}
}
