//go:build !windows && !darwin

package main

// Linux / other non-macOS Unix secret encryption.
//
// TODO: integrate with the GNOME Keyring (libsecret) or KWallet via
// D-Bus so secrets are stored in the OS keychain. For now we use
// AES-256-GCM with a machine-derived key — same approach as the macOS
// stub. This is a stopgap that keeps the settings file encrypted at
// rest but is NOT a hardware-backed secret store.

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

func deriveKey() []byte {
	seed := platformStableMachineID()
	if seed == "" {
		seed = getHostname()
	}
	h := sha256.Sum256([]byte("PunMonitor-linux-secret-key-v1:" + seed))
	return h[:]
}
