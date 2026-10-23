package workflow

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"time"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/clientsdk/android"
	"feiyu.com/wx/clientsdk/baseinfo"
	"feiyu.com/wx/clientsdk/extinfo"
	"feiyu.com/wx/clientsdk/mmtls"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

// Request fields and the version-03 cipher are recovered from the target.
// The advertised client version comes from the verified workflow profile.
func fetchRecoveredDeviceToken(u *baseinfo.UserInfo) (*wechat.TrustResp, error) {
	table, err := recoveredTable()
	if err != nil {
		return nil, err
	}
	zt, err := android.NewRecoveredZT(table)
	if err != nil {
		return nil, err
	}
	device := clientsdk.Get62Key(u.LoginDataInfo.LoginData)
	if len(device) < 2 {
		return nil, errors.New("invalid device ID")
	}
	device = "49" + device[2:]
	uuid1, uuid2 := baseinfo.IOSUuid(device)
	d := u.DeviceInfo
	version := baseinfo.ClientVersion
	md5 := extinfo.MD5ToLower(device + extinfo.GetCid(0x262626262626))
	fields := [][2]string{
		{"deviceid", device}, {"sdi", "A136" + md5[5:]}, {"idfv", uuid1}, {"idfa", uuid2},
		{"device_model", d.IphoneVer}, {"os_version", d.OsType}, {"core_count", "6"},
		{"carrier_name", ""}, {"is_jailbreak", "0"}, {"device_name", d.DeviceName},
		{"client_version", strconv.FormatUint(uint64(version), 10)}, {"plist_version", strconv.FormatUint(uint64(version), 10)},
		{"language", "zh"}, {"locale_country", "CN"}, {"screen_width", "834"}, {"screen_height", "1112"},
		{"install_time", strconv.FormatInt(time.Now().Add(-2592234*time.Second).Unix(), 10)},
		{"kern_boottime", strconv.FormatInt(time.Now().Add(-2592230*time.Second).Unix()*1000, 10)},
	}
	td := &wechat.TrustReq{Td: &wechat.TrustData{}}
	for _, kv := range fields {
		td.Td.Tdi = append(td.Td.Tdi, &wechat.TrustDeviceInfo{Key: proto.String(kv[0]), Val: proto.String(kv[1])})
	}
	pb, err := proto.Marshal(td)
	if err != nil {
		return nil, err
	}
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write(pb); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	fp := &wechat.FPFresh{
		BaseRequest: &wechat.BaseRequest{SessionKey: []byte{}, Uin: proto.Uint32(0), DeviceId: append([]byte(device), 0), ClientVersion: &version, OsType: &d.OsType, Scene: proto.Uint32(0)},
		SessKey:     key,
		Ztdata:      &wechat.ZTData{Version: []byte("00000003"), Encrypted: proto.Uint64(1), Data: zt.EncryptRecovered(compressed.Bytes()), TimeStamp: proto.Int64(time.Now().Unix()), OpType: proto.Uint64(5), Uin: proto.Uint64(0)},
	}
	request, err := proto.Marshal(fp)
	if err != nil {
		return nil, err
	}
	hec := &android.Client{}
	hec.Init("IOS", int(version), "iPad")
	packed := hec.HybridEcdhPackIosEn(3789, 0, nil, request)
	response, err := mmtls.MMHTTPPostData(u.MMInfo, "/cgi-bin/micromsg-bin/fpinitnl", packed)
	if err != nil {
		return nil, err
	}
	if len(response) <= 31 {
		if len(response) >= 6 {
			return nil, fmt.Errorf("device packet error %d (%d bytes)", clientsdk.GetRespErrorCode(response), len(response))
		}
		return nil, fmt.Errorf("device response too short: %d bytes", len(response))
	}
	header := hec.HybridEcdhPackIosUn(response)
	if header == nil {
		return nil, errors.New("empty hybrid response")
	}
	result := &wechat.TrustResp{}
	if err := proto.Unmarshal(header.Data, result); err != nil {
		return nil, err
	}
	return result, nil
}
