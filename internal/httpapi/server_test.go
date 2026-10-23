package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"feiyu.com/wx/internal/workflow"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

const testToken = "0123456789abcdef0123456789abcdef"

func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(&workflow.Session{Dir: t.TempDir()}, Config{Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requestAPI(s *Server, method, path, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth {
		r.Header.Set("Authorization", "Bearer "+testToken)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestAuthAndMessageProjection(t *testing.T) {
	s := testServer(t)
	if got := requestAPI(s, "GET", "/v1/status", "", false); got.Code != 401 {
		t.Fatal(got.Code)
	}
	s.session.State.Pending = []*wechat.AddMsg{{NewMsgId: proto.Int64(9007199254740993), MsgType: proto.Uint32(3), Content: &wechat.SKBuiltinString{Str: proto.String("<msg><img aeskey=\"private-cdn-key\"/></msg>")}}}
	w := requestAPI(s, "GET", "/v1/messages", "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"9007199254740993"`) || strings.Contains(w.Body.String(), "private-cdn-key") {
		t.Fatal(w.Body.String())
	}
	if got := requestAPI(s, "GET", "/v1/messages?cursor=-1", "", true); got.Code != 400 {
		t.Fatal(got.Code)
	}
	if got := requestAPI(s, "GET", "/v1/media/../../session.json", "", true); got.Code != 404 {
		t.Fatal(got.Code)
	}
}

func TestIdempotencySurvivesRestartAndRejectsChangedRequests(t *testing.T) {
	s := testServer(t)
	calls := 0
	invoke := func(server *Server, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/messages/text", nil)
		r.Header.Set("Idempotency-Key", "same-message")
		w := httptest.NewRecorder()
		server.once(w, r, "text", []byte(body), func() (interface{}, error) { calls++; return map[string]string{"message_id": "9007199254740993"}, nil })
		return w
	}
	first := invoke(s, `{"to":"test","text":"hello"}`)
	if first.Code != 200 || calls != 1 {
		t.Fatal(first.Code, calls)
	}
	restarted, err := New(s.session, Config{Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	second := invoke(restarted, `{"to":"test","text":"hello"}`)
	if second.Code != 200 || calls != 1 || second.Header().Get("Idempotency-Replayed") != "true" || !bytes.Equal(bytes.TrimSpace(first.Body.Bytes()), bytes.TrimSpace(second.Body.Bytes())) {
		t.Fatal(second.Code, calls)
	}
	if changed := invoke(restarted, `{"to":"test","text":"changed"}`); changed.Code != 409 || calls != 1 {
		t.Fatal(changed.Code, calls)
	}
}

func TestInterruptedSubmissionIsNeverRetried(t *testing.T) {
	s := testServer(t)
	key := "interrupted"
	body := []byte(`{"to":"test"}`)
	entry := submission{State: "processing", RequestHash: fmt.Sprintf("%x", sha256.Sum256(append([]byte("text\n"), body...)))}
	data, _ := json.Marshal(entry)
	path := filepath.Join(s.session.Dir, "submissions", fmt.Sprintf("%x.json", sha256.Sum256([]byte(key))))
	if err := atomicWrite(path, data); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	s.once(w, r, "text", body, func() (interface{}, error) { t.Fatal("interrupted send retried"); return nil, nil })
	if w.Code != 409 || !strings.Contains(w.Body.String(), "delivery_unknown") {
		t.Fatal(w.Body.String())
	}
}

func TestUploadsArePrivateAndCannotReadArbitraryPaths(t *testing.T) {
	s := testServer(t)
	w := requestAPI(s, "POST", "/v1/uploads", "test image bytes", true)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var result struct {
		ID string `json:"upload_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	data, err := s.readUpload(result.ID, 1024)
	if err != nil || string(data) != "test image bytes" {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(s.session.Dir, "uploads", result.ID))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("upload permissions", err)
	}
	if _, err := s.readUpload("../../session.json", 1024); err == nil {
		t.Fatal("path traversal accepted")
	}
	if got := requestAPI(s, "POST", "/v1/messages/text", `{"to":"test","text":"hello","unexpected":true}`, true); got.Code != 400 {
		t.Fatal(got.Code)
	}
	if got := requestAPI(s, "POST", "/v1/messages/text", `{"to":"test","text":"hello"}`, true); got.Code != 400 {
		t.Fatal("missing idempotency key accepted", got.Code)
	}
}

func TestTokenPersistsPrivately(t *testing.T) {
	dir := t.TempDir()
	first, err := TokenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TokenFile(dir)
	if err != nil || first != second || len(first) != 64 {
		t.Fatal("token changed", err)
	}
	st, err := os.Stat(filepath.Join(dir, "api-token"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("token permissions", err)
	}
}

var _ http.Handler = (*Server)(nil)
