package android

import (
	"bytes"
	"encoding/base64"
	"os"
	"testing"
)

func TestRecoveredCipherMatchesReferenceBeforeCompression(t *testing.T) {
	table, err := base64.StdEncoding.DecodeString(SaeDat06)
	if err != nil {
		t.Fatal(err)
	}
	z, err := NewRecoveredZT(table)
	if err != nil {
		t.Fatal(err)
	}
	original := &ZT{}
	original.Init()
	for _, input := range [][]byte{nil, []byte("device payload"), bytes.Repeat([]byte{1, 2, 3, 4}, 200)} {
		compressed := zipCCData(input)
		got := z.EncryptRecovered(compressed)
		want := original.WBAesEncrypt(input)
		if !bytes.Equal(got, want) {
			t.Fatal("cipher differs from reference on identical table and compressed input")
		}
	}
}

func TestTargetTableAndInputOwnership(t *testing.T) {
	table, err := os.ReadFile("../../internal/workflow/target_assets/sae.pb")
	if err != nil {
		t.Fatal(err)
	}
	z, err := NewRecoveredZT(table)
	if err != nil {
		t.Fatal(err)
	}
	if z.ver != "00000003" {
		t.Fatalf("unexpected extracted version: %q", z.ver)
	}
	input := make([]byte, 17, 64)
	copy(input, []byte("preserve input"))
	before := append([]byte(nil), input[:cap(input)]...)
	first, second := z.EncryptRecovered(input), z.EncryptRecovered(input)
	if len(first) != 32 || !bytes.Equal(first, second) {
		t.Fatal("nondeterministic CBC output or incorrect padding")
	}
	if !bytes.Equal(input[:cap(input)], before) {
		t.Fatal("input backing storage changed")
	}
	if _, err := NewRecoveredZT([]byte{0xff}); err == nil {
		t.Fatal("corrupt table accepted")
	}
}
