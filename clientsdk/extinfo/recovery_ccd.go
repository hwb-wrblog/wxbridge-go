package extinfo

import (
	"crypto/hmac"
	"crypto/sha1"
	"errors"
	"fmt"
	"hash/crc32"
	"math/rand"
	"strings"
	"time"

	"feiyu.com/wx/clientsdk/android"
	"feiyu.com/wx/clientsdk/baseutils"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/gogf/guuid"
	"github.com/gogo/protobuf/proto"
)

// RecoveredCCDEncoder encrypts bytes that have already been zlib-compressed.
type RecoveredCCDEncoder func([]byte) ([]byte, error)

// NewRecoveredCCDEncoder uses the target's assets/sae.dat, not SaeDat06.
func NewRecoveredCCDEncoder(table []byte) (RecoveredCCDEncoder, error) {
	var info wechat.SaeInfoAndroid
	if err := proto.Unmarshal(table, &info); err != nil {
		return nil, err
	}
	if info.GetVer() != "00000003" {
		return nil, errors.New("recovered CCD requires version 00000003 table")
	}
	zt, err := android.NewRecoveredZT(table)
	if err != nil {
		return nil, err
	}
	return func(compressed []byte) ([]byte, error) {
		if len(compressed) == 0 {
			return nil, errors.New("recovered CCD cannot encode empty input")
		}
		out := zt.EncryptRecovered(compressed)
		// Target EncodeZipData at 0xac2730 skips a full padding block.
		if len(compressed)%16 == 0 {
			out = out[:len(out)-16]
		}
		return out, nil
	}, nil
}

func signRecoveredRqtBuf(input string) uint32 {
	// Static extinfo.RqtKey bytes at target ELF address 0x1a552e0.
	key := [...]byte{0x6a, 0x66, 0x4d, 0x5d, 0x53, 0x7c, 0x25, 0x3f, 0x73, 0x6e, 0x48, 0x27, 0x3a, 0x29, 0x5e, 0x4f}
	h := hmac.New(sha1.New, key[:])
	h.Write([]byte(input))
	digest := h.Sum(nil)
	var a, b, c uint32
	for i := 0; i < len(digest)-2; i++ {
		a = a*131 + uint32(digest[i])
		b = b*131 + uint32(digest[i+1])
		c = c*131 + uint32(digest[i+2])
	}
	return 0x21000000 | (a & 0x7f) | ((b & 0x7f) << 8) | ((c & 0x7f) << 16)
}

func marshalRecoveredCCD(message proto.Message, timestamp uint32, encode RecoveredCCDEncoder) ([]byte, error) {
	if encode == nil {
		return nil, errors.New("recovered CCD encoder is required")
	}
	payload, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	encrypted, err := encode(baseutils.CompressByteArray(payload))
	if err != nil {
		return nil, err
	}
	return proto.Marshal(&wechat.DeviceRunningInfoNew{
		Version:     []byte("00000003"),
		Type:        proto.Uint32(1),
		EncryptData: encrypted,
		Timestamp:   proto.Uint32(timestamp),
		Unknown5:    proto.Uint32(5),
		Unknown6:    proto.Uint32(0),
	})
}

// GetRecoveredCCDPbLib follows target GetCCDPbLib at 0xac5e60.
func GetRecoveredCCDPbLib(iosVersion, deviceType, uuid1, uuid2, deviceName string, deviceToken *wechat.TrustResp, deviceID, userName, guid2 string, encode RecoveredCCDEncoder) ([]byte, error) {
	cc, err := GetRecoveredNewSpamData(iosVersion, deviceType, uuid1, uuid2, deviceName, deviceToken, deviceID, userName, guid2, encode)
	if err != nil {
		return nil, err
	}
	wcstf, err := GetRecoveredWcstf(userName, encode)
	if err != nil {
		return nil, err
	}
	wcste, err := GetRecoveredWcste(encode)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(&wechat.WCExtInfoNew{
		Wcstf:  &wechat.BufferT{ILen: proto.Uint32(uint32(len(wcstf))), Buffer: wcstf},
		Wcste:  &wechat.BufferT{ILen: proto.Uint32(uint32(len(wcste))), Buffer: wcste},
		CcData: &wechat.BufferT{ILen: proto.Uint32(uint32(len(cc))), Buffer: cc},
	})
}

// GetRecoveredNewSpamData preserves the reference body except for the verified
// target hash, locale, envelope, and cipher changes. Other fields and helpers
// have not all been recovered field-by-field from the target binary.
func GetRecoveredNewSpamData(iosVersion, deviceType, uuid1, uuid2, deviceName string, deviceToken *wechat.TrustResp, deviceID, userName, guid2 string, encode RecoveredCCDEncoder) ([]byte, error) {
	if len(deviceID) < 29 {
		return nil, errors.New("recovered CCD device ID must contain at least 29 bytes")
	}
	dateTimeStamp := time.Now().Unix()
	timestamp := time.Now().Unix()
	xorKey := MakeXorKey(timestamp)
	guid1 := guuid.New().String()
	if guid2 == "" {
		guid2 = guuid.New().String()
	}
	rqtHash := baseutils.EncInt(int64(signRecoveredRqtBuf(baseutils.Md5Value("modify" + deviceID))))
	body := wechat.SpamDataBody{
		UnKnown1:              proto.Int32(1),
		TimeStamp:             proto.Uint32(uint32(dateTimeStamp)),
		KeyHash:               proto.Int32(int32(MakeKeyHash(xorKey))),
		Yes1:                  proto.String(XorEncrypt("yes", xorKey)),
		Yes2:                  proto.String(XorEncrypt("yes", xorKey)),
		IosVersion:            proto.String(XorEncrypt(iosVersion, xorKey)),
		DeviceType:            proto.String(XorEncrypt(deviceType, xorKey)),
		UnKnown2:              proto.Int32(4),
		IdentifierForVendor:   proto.String(XorEncrypt(uuid1, xorKey)),
		AdvertisingIdentifier: proto.String(XorEncrypt(uuid2, xorKey)),
		Carrier:               proto.String(XorEncrypt("\u4e2d\u56fd\u8054\u901a", xorKey)),
		BatteryInfo:           proto.Int32(1),
		NetworkName:           proto.String(XorEncrypt("en0", xorKey)),
		NetType:               proto.Int32(1),
		AppBundleId:           proto.String(XorEncrypt("com.tencent.xin", xorKey)),
		DeviceName:            proto.String(XorEncrypt(deviceName, xorKey)),
		UserName:              proto.String(XorEncrypt(userName, xorKey)),
		Unknown3:              proto.Int64(DeviceNumber(deviceID[:29] + "FFF")),
		Unknown4:              proto.Int64(DeviceNumber(deviceID[:29] + "OOO")),
		Unknown5:              proto.Int32(0),
		Unknown6:              proto.Int32(4),
		Lang:                  proto.String(XorEncrypt("zh", xorKey)),
		Country:               proto.String(XorEncrypt("CN", xorKey)),
		Unknown7:              proto.Int32(4),
		DocumentDir:           proto.String(XorEncrypt(fmt.Sprintf("/var/mobile/Containers/Data/Application/%s/Documents", guid1), xorKey)),
		Unknown8:              proto.Int32(0),
		Unknown9:              proto.Int32(0),
		HeadMD5:               proto.String(XorEncrypt(GetCidMd5(deviceID, GetCid(0x0262626262626)), xorKey)),
		AppUUID:               proto.String(XorEncrypt(uuid1, xorKey)),
		SyslogUUID:            proto.String(XorEncrypt(uuid2, xorKey)),
		Unknown10:             proto.String(XorEncrypt(BuildRandomWifiSsid(), xorKey)),
		Unknown11:             proto.String(XorEncrypt(baseutils.BuildRandomMac(), xorKey)),
		AppName:               proto.String(XorEncrypt("\u5fae\u4fe1", xorKey)),
		SshPath:               proto.String(XorEncrypt("/usr/bin/ssh", xorKey)),
		TempTest:              proto.String(XorEncrypt("/tmp/test.txt", xorKey)),
		DevMD5:                proto.String(""),
		DevUser:               proto.String(""),
		DevPrefix:             proto.String(""),
		AppFileInfo:           GetFileInfo(deviceID, xorKey, guid2),
		Unknown12:             proto.String(""),
		IsModify:              proto.Int32(0),
		ModifyMD5:             proto.String(baseutils.Md5Value("modify")),
		RqtHash:               proto.Int64(rqtHash),
		Unknown53:             proto.Uint64(1586355322),
		Unknown54:             proto.Uint64(uint64(dateTimeStamp)),
		Unknown55:             proto.Uint64(0),
		Unknown56:             proto.Int64(rqtHash),
		Unknown57:             proto.Uint64(0),
		Unknown58:             proto.String(XorEncrypt(deviceID, xorKey)),
		Unknown59:             proto.String(fmt.Sprintf("%v", baseutils.EncInt(int64(crc32.ChecksumIEEE([]byte(XorEncrypt(deviceID, xorKey))))))),
		Unknown61:             proto.String(XorEncrypt("2FFC7F6DFEEFFF2B3FFCA029", xorKey)),
		Unknown62:             proto.Uint64(1175744137544159509),
		Unknown63:             proto.String(XorEncrypt(baseutils.Md5Value(deviceID+"imsi"), xorKey)),
	}
	filePath := []string{
		"/var/containers/Bundle/Application/" + GenGUId(deviceID, GetCid(0x098521236654)) + "/WeChat.app/WeChat",
		"/var/containers/Bundle/Application/" + GenGUId(deviceID, GetCid(0x098521236654)) + "/WeChat.app/Frameworks/mars.framework/mars",
		"/var/containers/Bundle/Application/" + GenGUId(deviceID, GetCid(0x098521236654)) + "/WeChat.app/Frameworks/andromeda.framework/andromeda",
		"/var/containers/Bundle/Application/" + GenGUId(deviceID, GetCid(0x098521236654)) + "/WeChat.app/Frameworks/OpenSSL.framework/OpenSSL",
		"/var/containers/Bundle/Application/" + GenGUId(deviceID, GetCid(0x098521236654)) + "/WeChat.app/Frameworks/ProtobufLite.framework/ProtobufLite",
		"/var/containers/Bundle/Application/" + GenGUId(deviceID, GetCid(0x098521236654)) + "/WeChat.app/Frameworks/marsbridgenetwork.framework/marsbridgenetwork",
		"/var/containers/Bundle/Application/" + GenGUId(deviceID, GetCid(0x098521236654)) + "/WeChat.app/Frameworks/matrixreport.framework/matrixreport",
	}
	fileUUID := []string{
		"3A1D0388-6BDB-350C-8706-80E3D15AA7C7",
		"A7DA401B-3FF6-3920-A30A-1B0FA8258202",
		"10F1245A-68FD-310D-98B3-0CFD51760BDE",
		"8FAE149B-602B-3B9D-A620-88EA75CE153F",
		"05BD590C-4DF6-3EDB-8316-0C9783928DD0",
		"CFED9A03-C881-3D50-B014-732D0A09879F",
		"1E7F06D2-DD36-31A8-AF3B-00D62054E1F9",
	}
	var appFile []string
	for i := 0; i < len(filePath); i++ {
		appFile = append(appFile, filePath[i], fileUUID[i])
	}
	encInt := baseutils.EncInt(int64(crc32.ChecksumIEEE([]byte(strings.Join(appFile, "")))))
	body.Unknown60 = proto.String(fmt.Sprintf("%v", encInt))
	body.Unknown64 = proto.String(XorEncrypt(deviceToken.GetTrustResponseData().GetDeviceToken(), xorKey))
	data, err := proto.Marshal(&body)
	if err != nil {
		return nil, err
	}
	return marshalRecoveredCCD(&wechat.NewClientCheckData{
		C32CData:  proto.Int64(int64(crc32.ChecksumIEEE(data))),
		TimeStamp: proto.Int64(time.Now().Unix()),
		DataBody:  data,
	}, uint32(timestamp), encode)
}

func GetRecoveredWcstf(userName string, encode RecoveredCCDEncoder) ([]byte, error) {
	now := time.Now().UnixNano() / 1000000
	endTimes := make([]uint64, 0, len(userName))
	endTime := now
	for i := 0; i < len(userName); i++ {
		endTime += rand.Int63n(10000)
		endTimes = append(endTimes, uint64(endTime))
	}
	return marshalRecoveredCCD(&wechat.WCSTF{
		StartTime: proto.Uint64(uint64(now)),
		CheckTime: proto.Uint64(uint64(now)),
		Count:     proto.Uint32(uint32(len(userName))),
		EndTime:   endTimes,
	}, uint32(now), encode)
}

func GetRecoveredWcste(encode RecoveredCCDEncoder) ([]byte, error) {
	now := time.Now().Unix()
	milliseconds := uint64(time.Now().UnixNano() / 1000000)
	return marshalRecoveredCCD(&wechat.WCSTE{
		CheckId:   proto.String("<LoginByID>"),
		StartTime: proto.Uint32(uint32(now)),
		CheckTime: proto.Uint32(uint32(now)),
		Count1:    proto.Uint32(0),
		Count2:    proto.Uint32(1),
		Count3:    proto.Uint32(0),
		Const1:    proto.Uint64(384214787666497617),
		Const2:    proto.Uint64(milliseconds),
		Const3:    proto.Uint64(milliseconds),
		Const4:    proto.Uint64(milliseconds),
		Const5:    proto.Uint64(milliseconds),
		Const6:    proto.Uint64(384002236977512448),
	}, uint32(now), encode)
}
