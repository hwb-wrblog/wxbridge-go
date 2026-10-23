package workflow

import (
	"encoding/hex"
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

func recoveredAuthDeviceRequest(u *baseinfo.UserInfo, now time.Time) (*wechat.ManualAuthAesReqData, error) {
	if u == nil || u.DeviceInfo == nil {
		return nil, errors.New("manual authentication requires an iPad device")
	}
	table, err := recoveredTable()
	if err != nil {
		return nil, err
	}
	encode, err := extinfo.NewRecoveredCCDEncoder(table)
	if err != nil {
		return nil, err
	}
	d := u.DeviceInfo
	spam, err := extinfo.GetRecoveredCCDPbLib(d.OsTypeNumber, d.OsType,
		d.UUIDTwo, d.UUIDTwo, d.DeviceName, d.DeviceToken,
		hex.EncodeToString(d.DeviceID), u.GetUserName(), d.GUID2, encode)
	if err != nil {
		return nil, fmt.Errorf("build recovered CCD: %w", err)
	}
	base := clientsdk.GetBaseRequest(u)
	base.Scene, base.SessionKey = proto.Uint32(1), []byte{}
	// Target GetManualAuthAesReqDataMarshal at 0xafe520 has no ticket branch.
	return &wechat.ManualAuthAesReqData{
		BaseRequest: base, Imei: &d.Imei, SoftType: &d.SoftTypeXML,
		BuiltinIpseq: proto.Uint32(0), ClientSeqId: proto.String(d.Imei + "-" + strconv.FormatInt(now.Unix(), 10)),
		Signature: proto.String(""), DeviceName: &d.DeviceName, DeviceType: &d.DeviceName,
		Language: &d.Language, TimeZone: &d.TimeZone, Channel: proto.Int32(0),
		TimeStamp: proto.Uint32(uint32(now.Unix())), DeviceBrand: &d.DeviceBrand,
		RealCountry: &d.RealCountry, BundleId: &d.BundleID, AdSource: &d.AdSource,
		InputType:   proto.Uint32(2),
		ExtSpamInfo: &wechat.SKBuiltinString_{Len: proto.Uint32(uint32(len(spam))), Buffer: spam},
	}, nil
}

// Adapted from SendHybridManualAutoRequest2 with recovered CCD data.
// Verified with a fresh 8.0.75 QR and a test account on 2026-10-23.
func sendRecoveredHybridAuth(u *baseinfo.UserInfo, password, username string) (*baseinfo.PackHeader, error) {
	if u == nil || u.DeviceInfo == nil || len(u.SessionKey) != 16 {
		return nil, errors.New("hybrid authentication requires a QR session key and iPad device")
	}
	account := clientsdk.GetManualAuthRsaReqDataProtobuf(u, username, password)
	device, err := recoveredAuthDeviceRequest(u, time.Now())
	if err != nil {
		return nil, err
	}
	data, err := proto.Marshal(&wechat.ManualAuthRequest{RsaReqData: account, AesReqData: device})
	if err != nil {
		return nil, err
	}
	hec := &android.Client{}
	hec.Init("IOS", int(baseinfo.ClientVersion), baseinfo.DeviceTypeIos)
	packed := hec.HybridEcdhPackIosEn(252, 0, nil, data)
	response, err := mmtls.MMHTTPPostData(u.MMInfo, "/cgi-bin/micromsg-bin/secmanualauth", packed)
	if err != nil {
		return nil, err
	}
	if len(response) <= 32 {
		if len(response) < 6 {
			return nil, fmt.Errorf("truncated hybrid authentication response: %d bytes", len(response))
		}
		return nil, fmt.Errorf("hybrid authentication packet rejected: %d", clientsdk.GetRespErrorCode(response))
	}
	header := hec.HybridEcdhPackIosUn(response)
	if header == nil || len(header.Data) == 0 {
		return nil, errors.New("empty hybrid authentication response")
	}
	return &baseinfo.PackHeader{EncodeType: 12, Data: header.Data, Session: header.Cookies, Uin: header.Uin}, nil
}
