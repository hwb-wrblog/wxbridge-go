package extinfo

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"testing"

	"feiyu.com/wx/clientsdk/android"
	"feiyu.com/wx/clientsdk/baseutils"
	"feiyu.com/wx/clientsdk/ccdata"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/gogo/protobuf/proto"
)

func TestRecoveredRqtHashVectors(t *testing.T) {
	// Independently derived with Node/OpenSSL HMAC-SHA1 and integer arithmetic.
	for _, tc := range []struct {
		input string
		hash  uint32
		enc   int64
	}{
		{"00000000000000000000000000000000", 0x2135625d, 288517422036816209},
		{"0123456789abcdef0123456789abcdef", 0x21596e59, 288530822340022593},
		{"2066f2dee0e63690b19f44fb6988ace4", 0x214d3a56, 288529791295164692},
	} {
		if got := signRecoveredRqtBuf(tc.input); got != tc.hash {
			t.Fatalf("sign(%q) = %#x, want %#x", tc.input, got, tc.hash)
		}
		if got := baseutils.EncInt(int64(tc.hash)); got != tc.enc {
			t.Fatalf("EncInt(%#x) = %d, want %d", tc.hash, got, tc.enc)
		}
	}
}

func recoveredCCDTable(t *testing.T) []byte {
	t.Helper()
	table, err := os.ReadFile("../../internal/workflow/target_assets/sae.pb")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(table)); got != "eb819dc4c677c5f4996aa8b4ecc0de33863e9f7f546a2b69b5ec92094cee79fb" {
		t.Fatalf("unexpected target CCD table SHA256: %s", got)
	}
	return table
}

// Uses the separate ccdata table implementation without changing its globals.
// The 0x3060 control flow, including aligned padding, matches target 0xac2680.
func referenceCCDEncode(input []byte, table *wechat.SaeInfo) []byte {
	data := append([]byte(nil), input...)
	if padding := 16 - len(data)%16; padding < 16 {
		data = append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
	}
	iv := table.GetIv()
	result := make([]byte, 0, len(data))
	for offset := 0; offset < len(data); offset += 16 {
		xor := make([]byte, 16)
		for i := range xor {
			xor[i] = data[offset+i] ^ iv[i]
		}
		state := make([]byte, 16)
		for row := 0; row < 4; row++ {
			for col := 0; col < 4; col++ {
				state[row*4+col] = xor[col*4+row]
			}
		}
		state = ccdata.ShiftRows(state)
		for round := 0; round < 9; round++ {
			sec := ccdata.GetSecTable(make([]byte, 64), state, table.GetTableKey(), round*0x4000)
			state = ccdata.GetSecValue(state, sec, table.GetTableValue(), round*0x3000)
			state = ccdata.ShiftRows(state)
		}
		state = ccdata.GetSecValueFinal(state, table.GetUnknowValue18())
		iv = make([]byte, 16)
		for row := 0; row < 4; row++ {
			for col := 0; col < 4; col++ {
				iv[row+4*col] = state[row*4+col]
			}
		}
		result = append(result, iv...)
	}
	return result
}

func TestRecoveredCCDCipherMatchesSeparateTableImplementation(t *testing.T) {
	table := recoveredCCDTable(t)
	encode, err := NewRecoveredCCDEncoder(table)
	if err != nil {
		t.Fatal(err)
	}
	var info wechat.SaeInfo
	if err := proto.Unmarshal(table, &info); err != nil {
		t.Fatal(err)
	}
	zt, err := android.NewRecoveredZT(table)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1, 2, 15, 16, 17, 31, 32, 33, 255, 256, 257, 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			input := make([]byte, size, size+32)
			for i := range input {
				input[i] = byte(i*73 + size)
			}
			before := append([]byte(nil), input[:cap(input)]...)
			got, err := encode(input)
			if err != nil {
				t.Fatal(err)
			}
			want := referenceCCDEncode(input, &info)
			if !bytes.Equal(got, want) {
				t.Fatal("recovered ZT and ccdata disagree with the same target table")
			}
			if wantLen := (size + 15) / 16 * 16; len(got) != wantLen {
				t.Fatalf("ciphertext length = %d, want %d", len(got), wantLen)
			}
			unadapted := zt.EncryptRecovered(input)
			if size%16 == 0 && len(unadapted) != len(got)+16 {
				t.Fatal("aligned CCD input must omit ZT's extra PKCS7 block")
			}
			if !bytes.Equal(input[:cap(input)], before) {
				t.Fatal("encoder changed caller backing storage")
			}
			again, err := encode(input)
			if err != nil || !bytes.Equal(got, again) {
				t.Fatal("encoder retained CBC state across calls")
			}
		})
	}
	if _, err := encode(nil); err == nil {
		t.Fatal("empty input accepted")
	}
}

func decodeRecoveredEnvelope(t *testing.T, buffer *wechat.BufferT, message proto.Message) *wechat.DeviceRunningInfoNew {
	t.Helper()
	if buffer == nil || int(buffer.GetILen()) != len(buffer.GetBuffer()) {
		t.Fatal("missing or invalid outer buffer length")
	}
	var envelope wechat.DeviceRunningInfoNew
	if err := proto.Unmarshal(buffer.GetBuffer(), &envelope); err != nil {
		t.Fatal(err)
	}
	if string(envelope.GetVersion()) != "00000003" || envelope.GetType() != 1 || envelope.GetUnknown5() != 5 || envelope.GetUnknown6() != 0 {
		t.Fatal("incorrect CCD envelope metadata")
	}
	r, err := zlib.NewReader(bytes.NewReader(envelope.GetEncryptData()))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(payload, message); err != nil {
		t.Fatal(err)
	}
	return &envelope
}

func TestRecoveredCCDEnvelopeAndSpamFields(t *testing.T) {
	const deviceID = "0123456789abcdef0123456789abcdef"
	const userName = "offline_test_user"
	token := &wechat.TrustResp{TrustResponseData: &wechat.TrustResponseData{DeviceToken: proto.String("offline-test-token")}}
	identity := func(compressed []byte) ([]byte, error) { return append([]byte(nil), compressed...), nil }
	data, err := GetRecoveredCCDPbLib("17.0", "iPad", "uuid1", "uuid2", "iPad", token, deviceID, userName, "guid2", identity)
	if err != nil {
		t.Fatal(err)
	}
	var outer wechat.WCExtInfoNew
	if err := proto.Unmarshal(data, &outer); err != nil {
		t.Fatal(err)
	}
	if outer.UserAttrInfo != nil || outer.AcgiDeviceInfo != nil || outer.AcgiTuring != nil || outer.DeviceToken != nil || outer.BehaviorId != nil || outer.IosturingHuman != nil || outer.IosturingOwner != nil || len(outer.XXX_unrecognized) != 0 {
		t.Fatal("outer CCD contains fields other than 1, 2, 3")
	}
	var cc wechat.NewClientCheckData
	envelope := decodeRecoveredEnvelope(t, outer.CcData, &cc)
	if cc.GetC32CData() != int64(crc32.ChecksumIEEE(cc.GetDataBody())) {
		t.Fatal("spam-body CRC is incorrect")
	}
	var body wechat.SpamDataBody
	if err := proto.Unmarshal(cc.GetDataBody(), &body); err != nil {
		t.Fatal(err)
	}
	key := byte(uint64(envelope.GetTimestamp())*0xffffffed + 7)
	decodeXOR := func(value string) string {
		var decoded []byte
		for _, r := range value {
			decoded = append(decoded, byte(r)^key)
		}
		return string(decoded)
	}
	if decodeXOR(body.GetLang()) != "zh" || decodeXOR(body.GetCountry()) != "CN" {
		t.Fatal("spam-body locale must be zh/CN")
	}
	if decodeXOR(body.GetUnknown64()) != "offline-test-token" {
		t.Fatal("removing outer token also removed or changed inner token")
	}
	if body.GetRqtHash() != 288529791295164692 || body.GetUnknown56() != 288529791295164692 {
		t.Fatal("spam-body hash differs from independent fixed-device vector")
	}
	var wcstf wechat.WCSTF
	decodeRecoveredEnvelope(t, outer.Wcstf, &wcstf)
	if wcstf.GetCount() != uint32(len(userName)) || len(wcstf.GetEndTime()) != len(userName) {
		t.Fatal("WCSTF count differs from username byte length")
	}
	var wcste wechat.WCSTE
	decodeRecoveredEnvelope(t, outer.Wcste, &wcste)
	if wcste.GetCheckId() != "<LoginByID>" || wcste.GetCount2() != 1 {
		t.Fatal("WCSTE login fields differ")
	}
}

func TestRecoveredCCDRejectsInvalidInputsAndPropagatesEncoderErrors(t *testing.T) {
	for _, table := range [][]byte{nil, {0xff}} {
		if _, err := NewRecoveredCCDEncoder(table); err == nil {
			t.Fatal("invalid table accepted")
		}
	}
	for _, version := range []string{"00000003", "00000006"} {
		table, err := proto.Marshal(&wechat.SaeInfoAndroid{Ver: proto.String(version)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewRecoveredCCDEncoder(table); err == nil {
			t.Fatal("table with invalid version or missing keys accepted")
		}
	}
	if _, err := GetRecoveredNewSpamData("", "", "", "", "", nil, "short", "", "", nil); err == nil {
		t.Fatal("short device ID accepted")
	}
	if _, err := GetRecoveredWcste(nil); err == nil {
		t.Fatal("missing encoder accepted")
	}
	for failAt := 1; failAt <= 3; failAt++ {
		calls := 0
		failure := errors.New("offline encoder failure")
		encode := func(payload []byte) ([]byte, error) {
			calls++
			if calls == failAt {
				return nil, failure
			}
			return payload, nil
		}
		_, err := GetRecoveredCCDPbLib("", "", "", "", "", nil, "0123456789abcdef0123456789abcdef", "", "", encode)
		if !errors.Is(err, failure) || calls != failAt {
			t.Fatalf("encoder error %d not propagated: %v after %d calls", failAt, err, calls)
		}
	}
}
