package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMediaArgumentsCannotFallThroughToSending(t *testing.T) {
	for _, args := range [][]string{
		{"image"}, {"image", "-to", "test", "-unknown"},
		{"emoji", "-to", "test", "-size", "2147483648"},
		{"voice", "-to", "test", "-duration-ms", "60001"},
		{"video", "-to", "test", "-seconds", "-1"},
		{"video", "-to", "test", "-seconds", "2", "unexpected"},
	} {
		if err := run(nil, args, map[string]interface{}{}); err == nil {
			t.Fatalf("invalid media args accepted: %q", args)
		}
	}
}

func TestMediaFileReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMediaFile(path, 4); err == nil {
		t.Fatal("oversize media accepted")
	}
	if got, err := readMediaFile(path, 5); err != nil || string(got) != "12345" {
		t.Fatalf("exact-size media rejected: %q %v", got, err)
	}
	if _, err := readMediaFile(filepath.Dir(path), 10); err == nil {
		t.Fatal("directory accepted as media")
	}
}
