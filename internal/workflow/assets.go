package workflow

import (
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
)

// The ELF SeaDatAndroid and the container's /app/assets/sae.dat have the
// same SHA-256. Embedding the recovered data keeps builds self-contained.
//
//go:embed target_assets/sae.pb
var recoveredSAE []byte

func recoveredTable() ([]byte, error) {
	if fmt.Sprintf("%x", sha256.Sum256(recoveredSAE)) != "eb819dc4c677c5f4996aa8b4ecc0de33863e9f7f546a2b69b5ec92094cee79fb" {
		return nil, errors.New("recovered SAE table checksum mismatch")
	}
	return recoveredSAE, nil
}
