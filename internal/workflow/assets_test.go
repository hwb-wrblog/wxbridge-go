package workflow

import (
	"bytes"
	"testing"

	"feiyu.com/wx/clientsdk/android"
	"feiyu.com/wx/clientsdk/ccdata"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

func TestRecoveredCCDMatchesIndependentCipher(t *testing.T) {
	data, err := recoveredTable()
	if err != nil {
		t.Fatal(err)
	}
	var table wechat.SaeInfo
	if err := proto.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	previous := *ccdata.GetSaeInfo()
	*ccdata.GetSaeInfo() = table
	t.Cleanup(func() { *ccdata.GetSaeInfo() = previous })
	zt, err := android.NewRecoveredZT(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1, 15, 16, 17, 31, 32, 64} {
		input := make([]byte, size)
		for i := range input {
			input[i] = byte(i)
		}
		got, err := ccdata.EncodeZipData(input, 0x3060)
		if err != nil {
			t.Fatal(err)
		}
		want := zt.EncryptRecovered(input)
		// EncodeZipData omits a padding block for aligned input; ZT adds it.
		length := (size + 15) / 16 * 16
		if len(got) != length || !bytes.Equal(got, want[:length]) {
			t.Fatalf("CCD and ZT ciphers differ for %d input bytes", size)
		}
	}
}
