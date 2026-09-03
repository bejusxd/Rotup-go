package rclone

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"

	"github.com/bejusxd/rotup/internal/domain"
)

// rclone stores password fields in its configuration file in an obscured form
// and refuses plaintext. The transformation is rclone's own published, fixed-key
// AES-CTR obfuscation: it is deliberately reversible and provides no
// confidentiality. ROTUP implements it here only so that the transient
// configuration file can be written without ever passing the password on a
// command line or through `rclone obscure`, whose argument vector would be
// visible to other processes.
//
// The real protection for this material is the same as for the secret store:
// restrictive file permissions in a private directory, and deletion as soon as
// the child process exits.
var obscureKey = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// obscure converts plaintext into the form rclone expects in a config file.
func obscure(plaintext string) (string, error) {
	block, err := aes.NewCipher(obscureKey)
	if err != nil {
		return "", domain.WrapError(domain.ErrClassInternal, "rclone_obscure_failed",
			"the engine configuration could not be prepared", err)
	}
	buffer := make([]byte, aes.BlockSize+len(plaintext))
	iv := buffer[:aes.BlockSize]
	if _, err := rand.Read(iv); err != nil {
		return "", domain.WrapError(domain.ErrClassInternal, "rclone_obscure_failed",
			"the engine configuration could not be prepared", err)
	}
	cipher.NewCTR(block, iv).XORKeyStream(buffer[aes.BlockSize:], []byte(plaintext))
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// reveal inverts obscure. It exists so the transformation can be proven correct
// in tests and cross-checked against a real rclone binary during the safety
// spike; production code never calls it.
func reveal(obscured string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(obscured)
	if err != nil {
		return "", domain.WrapError(domain.ErrClassInternal, "rclone_reveal_failed",
			"the engine configuration could not be decoded", err)
	}
	if len(raw) < aes.BlockSize {
		return "", domain.NewError(domain.ErrClassInternal, "rclone_reveal_failed",
			"the engine configuration could not be decoded")
	}
	block, err := aes.NewCipher(obscureKey)
	if err != nil {
		return "", domain.WrapError(domain.ErrClassInternal, "rclone_reveal_failed",
			"the engine configuration could not be decoded", err)
	}
	plaintext := make([]byte, len(raw)-aes.BlockSize)
	cipher.NewCTR(block, raw[:aes.BlockSize]).XORKeyStream(plaintext, raw[aes.BlockSize:])
	return string(plaintext), nil
}
