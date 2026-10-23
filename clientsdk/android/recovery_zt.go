package android

import (
	"errors"
	"feiyu.com/wx/protobuf/wechat"
	"github.com/golang/protobuf/proto"
)

// NewRecoveredZT uses the table extracted from the target's SeaDatAndroid.
// Target ZT.Encrypt consumes already compressed bytes; WBAesEncrypt compresses again.
func NewRecoveredZT(data []byte) (*ZT, error) {
	p := &wechat.SaeInfoAndroid{}
	if err := proto.Unmarshal(data, p); err != nil {
		return nil, err
	}
	if len(p.GetInitKey()) != 16 || len(p.GetKey1()) != 9*0x4000 || len(p.GetKey2()) != 9*0x3000 || len(p.GetKey3()) != 0x1000 {
		return nil, errors.New("invalid recovered white-box table")
	}
	return &ZT{ver: p.GetVer(), initKey: p.GetInitKey(), key1: p.GetKey1(), key2: p.GetKey2(), key3: p.GetKey3()}, nil
}

func (z *ZT) EncryptRecovered(input []byte) []byte {
	pad := 16 - len(input)%16
	in := make([]byte, len(input)+pad)
	copy(in, input)
	for i := len(input); i < len(in); i++ {
		in[i] = byte(pad)
	}
	iv := z.initKey
	result := make([]byte, len(in))
	for offset := 0; offset < len(in); offset += 16 {
		var transposed [4][4]byte
		for i := 0; i < 16; i++ {
			transposed[i%4][i/4] = in[offset+i] ^ iv[i]
		}
		for round := 0; round < 9; round++ {
			shifted := z.shiftKey(transposed)
			keyed := z.chooseKey(shifted, z.key1[round*0x4000:])
			transposed = z.byte2Array(z.chooseKey2(keyed, z.key2[round*0x3000:]))
		}
		last := z.chooseKey3(z.shiftKey(transposed), z.key3)
		iv = z.reAssemble(last)
		copy(result[offset:], iv)
	}
	return result
}
