package workflow

import (
	"testing"
	"time"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func TestManualAuthUsesRecoveredCCD(t *testing.T) {
	if _, err := Open(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	u := clientsdk.NewUserInfo("490123456789abcdef0123456789abcd", "", nil)
	applyDeviceProfile(u)
	u.LoginDataInfo.UserName = "test-user"
	u.Ticket = "unused-historical-ticket"
	u.DeviceInfo.DeviceToken = &wechat.TrustResp{
		TrustResponseData: &wechat.TrustResponseData{DeviceToken: proto.String("offline-test-token")},
	}
	now := time.Unix(1700000000, 0)
	r, err := recoveredAuthDeviceRequest(u, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.GetBaseRequest().GetClientVersion() != ClientVersion || r.GetBaseRequest().GetOsType() != "iPad iOS18.6" {
		t.Fatal("manual authentication profile mismatch")
	}
	if r.GetInputType() != 2 || r.BaseReqInfo != nil || r.GetBaseRequest().GetScene() != 1 || len(r.GetBaseRequest().GetSessionKey()) != 0 {
		t.Fatal("manual authentication target fields mismatch")
	}
	if r.GetTimeStamp() != uint32(now.Unix()) || r.GetDeviceType() != u.DeviceInfo.DeviceName {
		t.Fatal("manual authentication device fields mismatch")
	}
	var ext wechat.WCExtInfoNew
	if err := proto.Unmarshal(r.GetExtSpamInfo().GetBuffer(), &ext); err != nil {
		t.Fatal(err)
	}
	if ext.DeviceToken != nil || ext.BehaviorId != nil {
		t.Fatal("historical outer fields leaked into target CCD")
	}
	for _, field := range []*wechat.BufferT{ext.Wcstf, ext.Wcste, ext.CcData} {
		if field == nil || int(field.GetILen()) != len(field.GetBuffer()) {
			t.Fatal("missing or truncated CCD field")
		}
		var envelope wechat.DeviceRunningInfoNew
		if err := proto.Unmarshal(field.GetBuffer(), &envelope); err != nil {
			t.Fatal(err)
		}
		if string(envelope.GetVersion()) != "00000003" || len(envelope.GetEncryptData()) == 0 || len(envelope.GetEncryptData())%16 != 0 {
			t.Fatal("invalid recovered CCD envelope")
		}
	}
}
