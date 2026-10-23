// Package workflow is a new test harness for the historical SDK. It is not
// an implementation of the target backend's authorization or HTTP contracts.
package workflow

import (
	"bytes"
	"crypto/elliptic"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"feiyu.com/wx/api/utils"
	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/clientsdk/baseinfo"
	"feiyu.com/wx/clientsdk/baseutils"
	"feiyu.com/wx/clientsdk/ccdata"
	"feiyu.com/wx/clientsdk/mmtls"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

type State struct {
	ClientVersion uint32                    `json:"client_version,omitempty"`
	User          *baseinfo.UserInfo        `json:"user"`
	QRKey         []byte                    `json:"qr_key"`
	QRState       uint32                    `json:"qr_state"`
	Expires       time.Time                 `json:"expires"`
	Notify        *wechat.LoginQRCodeNotify `json:"notify,omitempty"`
	Authenticated bool                      `json:"authenticated"`
	Pending       []*wechat.AddMsg          `json:"pending,omitempty"`
}

type Session struct {
	Dir   string
	State State
}

func Open(dir string) (*Session, error) {
	configureClient()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	s := &Session{Dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if os.IsNotExist(err) {
		s.State.ClientVersion = ClientVersion
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.State); err != nil {
		return nil, fmt.Errorf("invalid session file: %w", err)
	}
	if s.State.User == nil || s.State.User.DeviceInfo == nil {
		return nil, errors.New("invalid session: missing device")
	}
	if s.State.ClientVersion != ClientVersion {
		return nil, errors.New("legacy session profile is unsupported; create a fresh QR in a new state directory")
	}
	s.State.User.MMInfo = nil
	if !s.State.Authenticated {
		applyDeviceProfile(s.State.User)
	}
	return s, nil
}

func applyDeviceProfile(u *baseinfo.UserInfo) {
	// createDeviceInfo at 0xb1a340 uses "iPad iOS" + IPadOsVersion (18.6).
	d := u.DeviceInfo
	d.OsTypeNumber, d.OsType, d.CoreCount = "18.6", "iPad iOS18.6", 2
	d.SoftTypeXML = clientsdk.CreateSoftInfoXML(d)
	d.ClientCheckDataXML = ccdata.CreateClientCheckDataXML(d)
}

func (s *Session) Save() error {
	state := s.State
	if state.User != nil {
		u := *state.User
		u.MMInfo, u.Dialer = nil, nil
		state.User = &u
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.Dir, "session.json"))
}

func Handshake(host string) (*mmtls.MMInfo, error) {
	mm := mmtls.CreateNewMMInfo()
	mm.ShortHost = host
	mm.ShortURL = "/mmtls/" + baseutils.RandomSmallHexString(8)
	mm.ClientEcdhKeys = mmtls.CreateClientEcdhKeys()
	hello := mmtls.CreateHandShakeClientHelloData(mm)
	data, err := mmtls.MMHTTPPost(mm, mmtls.CreateRecordData(mmtls.ServerHandShakeType, hello))
	if err != nil {
		return nil, fmt.Errorf("handshake transport: %w", err)
	}
	items, err := mmtls.ParserMMtlsResponseData(data)
	if err != nil {
		return nil, err
	}
	if len(items) < 4 {
		return nil, fmt.Errorf("incomplete handshake: %d records", len(items))
	}
	if _, err := mmtls.DealHandShakePackItems(mm, items, hello); err != nil {
		return nil, fmt.Errorf("handshake verification: %w", err)
	}
	if len(mm.PskAccessKey) != 32 || len(mm.ShortPskList) == 0 {
		return nil, errors.New("handshake did not establish session keys")
	}
	return mm, nil
}

func (s *Session) connect() error {
	if s.State.User == nil {
		return errors.New("no session; run qr first")
	}
	if s.State.User.MMInfo != nil {
		return nil
	}
	mm, err := Handshake(s.State.User.ShortHost)
	if err != nil {
		return err
	}
	s.State.User.MMInfo = mm
	return nil
}

func responseOK(r *wechat.BaseResponse) error {
	if r == nil || r.Ret == nil {
		return errors.New("WeChat response has no result code")
	}
	if r.GetRet() != 0 {
		return fmt.Errorf("WeChat returned %d: %s", r.GetRet(), r.GetErrMsg().GetStr())
	}
	return nil
}

func (s *Session) QR() (string, error) {
	if s.State.Authenticated {
		return "", errors.New("session is already authenticated; use a different state directory for another account")
	}
	user := clientsdk.NewUserInfo(baseutils.RandomSmallHexString(32), "", nil)
	applyDeviceProfile(user)
	// The historical factory also creates an Android identity, which its sync
	// builder prefers. This harness uses the iPad path throughout.
	user.DeviceInfoA16 = nil
	user.LoginDataInfo.Language = "zh_CN"
	loginData, err := utils.GenerateWxDat(fmt.Sprintf("%x", user.DeviceInfo.DeviceID))
	if err != nil {
		return "", err
	}
	user.LoginDataInfo.LoginData = loginData
	s.State = State{User: user, ClientVersion: ClientVersion}
	if err := s.connect(); err != nil {
		return "", err
	}
	header, err := clientsdk.SendLoginQRCodeRequest(user)
	if err != nil {
		return "", err
	}
	qr := &wechat.LoginQRCodeResponse{}
	if err := clientsdk.ParseResponseData(user, header, qr); err != nil {
		return "", err
	}
	if err := responseOK(qr.GetBaseResponse()); err != nil {
		return "", err
	}
	if qr.GetUuid() == "" || len(qr.GetAes().GetKey()) != 16 {
		return "", errors.New("QR response lacks UUID or notification key")
	}
	img := qr.GetQrcode().GetSrc()
	_, format, err := image.DecodeConfig(bytes.NewReader(img))
	if err != nil {
		return "", fmt.Errorf("invalid QR image: %w", err)
	}
	path := filepath.Join(s.Dir, "qrcode."+format)
	if err := os.WriteFile(path, img, 0600); err != nil {
		return "", err
	}
	user.QrUuid = qr.GetUuid()
	s.State.QRKey = qr.GetAes().GetKey()
	// A missing/implausible server lifetime must not create an endless poll.
	lifetime := qr.GetExpiredTime()
	if lifetime == 0 || lifetime > 600 {
		lifetime = 240
	}
	s.State.Expires = time.Now().Add(time.Duration(lifetime) * time.Second)
	if err := s.Save(); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Session) Check() (uint32, error) {
	if s.State.Authenticated {
		return 2, nil
	}
	if s.State.User == nil || len(s.State.QRKey) != 16 {
		return 0, errors.New("no QR session; run qr first")
	}
	if time.Now().After(s.State.Expires) {
		return 4, errors.New("QR expired; run qr again")
	}
	if err := s.connect(); err != nil {
		return 0, err
	}
	u := s.State.User
	data, err := clientsdk.GetCheckLoginQRCodeReq(u, u.QrUuid)
	if err != nil {
		return 0, err
	}
	raw, err := mmtls.MMHTTPPostData(u.MMInfo, "/cgi-bin/micromsg-bin/checkloginqrcode", data)
	if err != nil {
		return 0, err
	}
	header, err := clientsdk.DecodePackHeader(raw, nil)
	if err != nil {
		return 0, err
	}
	check := &wechat.CheckLoginQRCodeResponse{}
	if err := clientsdk.ParseResponseData(u, header, check); err != nil {
		return 0, err
	}
	if err := responseOK(check.GetBaseResponse()); err != nil {
		return 0, err
	}
	plain, err := baseutils.AesDecryptByteKey(check.GetLoginQrcodeNotifyPkg().GetNotifyData().GetBuffer(), s.State.QRKey)
	if err != nil {
		return 0, err
	}
	notify := &wechat.LoginQRCodeNotify{}
	if err := proto.Unmarshal(plain, notify); err != nil {
		return 0, err
	}
	if notify.State == nil {
		return 0, errors.New("QR notification has no state")
	}
	s.State.Notify, s.State.QRState = notify, notify.GetState()
	if err := s.Save(); err != nil {
		return 0, err
	}
	return notify.GetState(), nil
}

func (s *Session) Login() error {
	if s.State.Authenticated {
		return s.Heartbeat()
	}
	n := s.State.Notify
	if n == nil || n.GetState() != 2 || n.GetWxid() == "" || n.GetWxnewpass() == "" {
		return errors.New("phone has not confirmed login; scan QR and run check first")
	}
	if err := s.connect(); err != nil {
		return err
	}
	u := s.State.User
	u.LoginDataInfo.NewPassWord, u.LoginDataInfo.UserName = n.GetWxnewpass(), n.GetWxid()
	token, err := fetchRecoveredDeviceToken(u)
	if err != nil {
		return fmt.Errorf("device token: %w", err)
	}
	if err := responseOK(token.GetBaseResponse()); err != nil {
		return fmt.Errorf("device token: %w", err)
	}
	if token.GetTrustResponseData().GetDeviceToken() == "" {
		return errors.New("empty device token")
	}
	u.DeviceInfo.DeviceToken = token
	if err := s.Save(); err != nil {
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		header, err := sendRecoveredHybridAuth(u, n.GetWxnewpass(), n.GetWxid())
		if err != nil {
			return fmt.Errorf("manual authentication: %w", err)
		}
		response := &wechat.ManualAuthResponse{}
		if err := clientsdk.ParseResponseData(u, header, response); err != nil {
			return err
		}
		if response.GetBaseResponse().GetRet() == baseinfo.MMErrIdcRedirect {
			host := ""
			for _, entry := range response.GetDnsInfo().GetBuiltinIplist().GetShortConnectIplist() {
				ip := strings.TrimRight(entry.GetIp(), "\x00")
				parsed := net.ParseIP(ip)
				if parsed != nil && parsed.IsGlobalUnicast() && !parsed.IsPrivate() {
					host = ip
					break
				}
			}
			if host == "" {
				return errors.New("login redirect has no public short-link address")
			}
			u.ShortHost, u.MMInfo = host, nil
			if err := s.connect(); err != nil {
				return err
			}
			continue
		}
		if err := responseOK(response.GetBaseResponse()); err != nil {
			return err
		}
		auth := response.GetAuthParam()
		pub := auth.GetEcdhKey().GetKey().GetBuffer()
		x, _ := elliptic.Unmarshal(elliptic.P224(), pub)
		if x == nil || len(auth.GetSessionKey().GetKey()) < 16 || response.GetAccountInfo().GetWxid() == "" {
			return errors.New("login returned incomplete account or invalid key exchange")
		}
		u.ConsultSessionKey(pub, auth.GetSessionKey().GetKey())
		if auth.GetUin() != 0 {
			u.Uin = uint32(auth.GetUin())
		}
		if u.Uin == 0 {
			return errors.New("login response has no account UIN")
		}
		u.WxId, u.NickName = response.GetAccountInfo().GetWxid(), response.GetAccountInfo().GetNickName()
		u.SetAutoKey(auth.GetAutoAuthKey().GetBuffer())
		u.LoginDataInfo.NewPassWord = ""
		s.State.Notify, s.State.QRKey = nil, nil
		s.State.Authenticated = true
		if err := s.Save(); err != nil {
			return err
		}
		if err := s.Heartbeat(); err != nil {
			return fmt.Errorf("authentication accepted but heartbeat failed: %w", err)
		}
		return nil
	}
	return errors.New("too many login redirects")
}

func (s *Session) Heartbeat() error {
	if !s.State.Authenticated {
		return errors.New("not authenticated")
	}
	if err := s.connect(); err != nil {
		return err
	}
	header, err := clientsdk.GetHeartBeatShortRequest(s.State.User)
	if err != nil {
		return err
	}
	response := &wechat.HeartBeatResponse{}
	if err := clientsdk.ParseResponseData(s.State.User, header, response); err != nil {
		return err
	}
	if err := responseOK(response.GetBaseResponse()); err != nil {
		return err
	}
	return s.Save()
}

func (s *Session) Send(to, content string) (*wechat.NewSendMsgResponse, error) {
	if !s.State.Authenticated {
		return nil, errors.New("not authenticated")
	}
	if strings.TrimSpace(to) == "" || strings.TrimSpace(content) == "" {
		return nil, errors.New("recipient and text are required")
	}
	if err := s.connect(); err != nil {
		return nil, err
	}
	header, err := clientsdk.SendTextMsg(s.State.User, to, content, nil, 1)
	if err != nil {
		return nil, err
	}
	response := &wechat.NewSendMsgResponse{}
	if err := clientsdk.ParseResponseData(s.State.User, header, response); err != nil {
		return nil, err
	}
	if err := ValidateSend(response, to); err != nil {
		return response, err
	}
	if err := s.Save(); err != nil {
		return response, fmt.Errorf("message sent, session save failed; do not retry blindly: %w", err)
	}
	return response, nil
}

func ValidateSend(response *wechat.NewSendMsgResponse, to string) error {
	if err := responseOK(response.GetBaseResponse()); err != nil {
		return err
	}
	items := response.GetChatSendRetList()
	if len(items) != 1 || response.GetCount() != 1 {
		return errors.New("send response does not acknowledge exactly one message")
	}
	item := items[0]
	if item == nil || item.Ret == nil || item.GetRet() != 0 {
		return errors.New("individual message was rejected or not acknowledged")
	}
	if item.GetToUserName().GetStr() != to {
		return errors.New("send acknowledgement recipient mismatch")
	}
	if item.GetNewMsgId() == 0 && item.GetMsgId() == 0 {
		return errors.New("send acknowledgement has no server message ID")
	}
	return nil
}

func (s *Session) Sync() ([]*wechat.AddMsg, bool, error) {
	if !s.State.Authenticated {
		return nil, false, errors.New("not authenticated")
	}
	if err := s.connect(); err != nil {
		return nil, false, err
	}
	header, err := clientsdk.SendNewSyncRequest(s.State.User, 3)
	if err != nil {
		return nil, false, err
	}
	response := &wechat.NewSyncResponse{}
	if err := clientsdk.ParseResponseData(s.State.User, header, response); err != nil {
		return nil, false, err
	}
	msgs, err := DecodeSync(response)
	if err != nil {
		return nil, false, err
	}
	if len(response.GetKeyBuf().GetBuffer()) == 0 {
		return nil, false, errors.New("sync response has no cursor")
	}
	s.State.User.SyncKey = response.GetKeyBuf().GetBuffer()
	s.State.Pending = append(s.State.Pending, msgs...)
	// Persist cursor and messages together so a crash does not lose a batch.
	if err := s.Save(); err != nil {
		return nil, false, err
	}
	return msgs, response.GetContinueFlag() != 0, nil
}

func DecodeSync(response *wechat.NewSyncResponse) ([]*wechat.AddMsg, error) {
	if response == nil || response.Ret == nil {
		return nil, errors.New("sync response has no result code")
	}
	if response.GetRet() != 0 {
		return nil, fmt.Errorf("sync returned %d", response.GetRet())
	}
	items := response.GetCmdList().GetItemList()
	if int(response.GetCmdList().GetCount()) != len(items) {
		return nil, errors.New("sync command count mismatch")
	}
	msgs := make([]*wechat.AddMsg, 0)
	for _, item := range items {
		if item == nil {
			return nil, errors.New("nil sync command")
		}
		if item.GetCmdId() != 5 {
			continue
		}
		msg := &wechat.AddMsg{}
		if err := proto.Unmarshal(item.GetCmdBuf().GetData(), msg); err != nil {
			return nil, fmt.Errorf("invalid incoming message: %w", err)
		}
		if msg.FromUserName == nil || msg.MsgType == nil {
			return nil, errors.New("incoming message has no sender or type")
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}
