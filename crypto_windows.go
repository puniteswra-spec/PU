//go:build windows

package main

// Windows DPAPI-based secret encryption.
//
// Uses CryptProtectData / CryptUnprotectData from Crypt32.dll. The encrypted
// data can only be decrypted by the SAME user on the SAME machine, which is
// exactly what we want for settings.json (a stolen laptop or a different
// user account should not be able to read the GitHub token or SSH host key).
//
// Format on disk: "enc:dpapi:<base64-ciphertext>"
// Plaintext is stored without any prefix, so users who hand-edit settings
// still get a working setup; load() will detect and pass through plaintext.

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	crypt32                  = windows.NewLazySystemDLL("Crypt32.dll")
	procCryptProtectData     = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData   = crypt32.NewProc("CryptUnprotectData")
	procLocalFree            = windows.NewLazySystemDLL("kernel32.dll").NewProc("LocalFree")
)

// dataBlob mirrors the Win32 DATA_BLOB struct.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(data []byte) *dataBlob {
	if len(data) == 0 {
		return &dataBlob{}
	}
	d := make([]byte, len(data))
	copy(d, data)
	return &dataBlob{cbData: uint32(len(d)), pbData: &d[0]}
}

func (b *dataBlob) bytes() []byte {
	if b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	for i := uint32(0); i < b.cbData; i++ {
		out[i] = *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(b.pbData)) + uintptr(i)))
	}
	return out
}

// encryptSecret DPAPI-encrypts a string and returns "enc:dpapi:<base64>".
// Empty input returns empty string (no encryption needed).
func encryptSecret(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if strings.HasPrefix(plaintext, "enc:dpapi:") {
		// Already encrypted (don't double-encrypt)
		return plaintext, nil
	}
	in := newBlob([]byte(plaintext))
	var out dataBlob
	// CRYPTPROTECT_UI_FORBIDDEN = 0x1 — never show UI
	r, _, errno := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(in)),
		0, 0, 0, 0,
		0x1,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return "", fmt.Errorf("CryptProtectData failed: errno %d", errno)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	enc := out.bytes()
	return "enc:dpapi:" + base64.StdEncoding.EncodeToString(enc), nil
}

// decryptSecret reverses encryptSecret. If the input doesn't have the
// "enc:dpapi:" prefix it's returned as-is (legacy plaintext support).
func decryptSecret(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if !strings.HasPrefix(ciphertext, "enc:dpapi:") {
		return ciphertext, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, "enc:dpapi:"))
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	in := newBlob(raw)
	var out dataBlob
	r, _, errno := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(in)),
		0, 0, 0, 0,
		0x1,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return "", fmt.Errorf("CryptUnprotectData failed: errno %d (settings may have been encrypted on a different user/machine)", errno)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return string(out.bytes()), nil
}

// isDPAPIAvailable returns true (always on Windows).
func isDPAPIAvailable() bool { return true }
