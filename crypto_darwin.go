//go:build darwin

package main

// macOS secret encryption.
//
// TODO: integrate with the macOS Keychain via the Security framework
// (CGO bindings to SecKeychainItemCopyContent / SecKeychainAddGenericPassword).
// For now we fall through to a machine-derived AES-256-GCM key so the
// settings file is still encrypted at rest (just not hardware-backed).
// Keychain integration is a follow-up — see TODO comment in main.go.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

func isDPAPIAvailable() bool { return false }

// encryptSecret returns "enc:aes:<base64(nonce||ciphertext)>" using a
// per-machine key derived from the stable machine ID. This is a
// best-effort obfuscation: it stops casual disk inspection but is NOT
// a hardware-backed secret store. For real keychain support see TODO
// in main.go.
func encryptSecret(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if strings.HasPrefix(plaintext, "enc:") {
		return plaintext, nil
	}
	key := deriveKey()
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return "enc:aes:" + base64.StdEncoding.EncodeToString(ct), nil
}

func decryptSecret(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if !strings.HasPrefix(ciphertext, "enc:aes:") {
		return ciphertext, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, "enc:aes:"))
	if err != nil {
		return "", err
	}
	key := deriveKey()
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// deriveKey returns a 32-byte AES key from the machine's stable ID.
// On macOS, platformStableMachineID() returns the SHA-1 of the first
// non-loopback MAC, prefixed by hostname. We use that to seed a key
// and stretch it with SHA-256.
func deriveKey() []byte {
	seed := platformStableMachineID()
	if seed == "" {
		seed = getHostname()
	}
	// Stretch the seed with a fixed salt for this use case.
	h := sha256.Sum256([]byte("PunMonitor-macOS-secret-key-v1:" + seed))
	return h[:]
}
