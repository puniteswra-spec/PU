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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	crypt32                = windows.NewLazySystemDLL("Crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = windows.NewLazySystemDLL("kernel32.dll").NewProc("LocalFree")
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
		return plaintext, nil
	}
	// In service mode (LocalSystem), skip DPAPI — settings live in
	// ProgramData which is protected by NTFS ACLs (SYSTEM only).
	if isServiceMode {
		return plaintext, nil
	}
	in := newBlob([]byte(plaintext))
	var out dataBlob
	flags := uint32(0x1) // CRYPTPROTECT_UI_FORBIDDEN
	r, _, errno := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(in)),
		0, 0, 0, 0,
		uintptr(flags),
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
	// Try user-level DPAPI first, then machine-level (for service mode)
	flags := uint32(0x1)
	for _, f := range []uint32{flags, flags | 0x4} {
		in := newBlob(raw)
		var out dataBlob
		r, _, _ := procCryptUnprotectData.Call(
			uintptr(unsafe.Pointer(in)),
			0, 0, 0, 0,
			uintptr(f),
			uintptr(unsafe.Pointer(&out)),
		)
		if r != 0 {
			result := string(out.bytes())
			procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
			return result, nil
		}
	}
	return "", fmt.Errorf("CryptUnprotectData failed (user and machine-level both rejected — settings may have been encrypted on a different machine)")
}

// isDPAPIAvailable returns true (always on Windows).
func isDPAPIAvailable() bool { return true }

// copySettingsToProgramData copies the user's settings.json (with DPAPI-encrypted
// secrets) to ProgramData (plaintext), so the service running as LocalSystem can
// read them. Called from --install-service before the service is installed.
func copySettingsToProgramData() {
	// Read user's settings (DPAPI-encrypted)
	userDataDir := func() string {
		if ad := os.Getenv("APPDATA"); ad != "" {
			return filepath.Join(ad, "PunMonitor")
		}
		return ""
	}()
	if userDataDir == "" {
		return
	}
	userSettings := filepath.Join(userDataDir, "settings.json")
	data, err := os.ReadFile(userSettings)
	if err != nil {
		llog("info", "No user settings to copy: %v", err)
		return
	}

	// Decrypt all enc:dpapi: values
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		llog("error", "Failed to parse user settings: %v", err)
		return
	}
	for k, v := range raw {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "enc:dpapi:") {
			decrypted, err := decryptSecret(s)
			if err != nil {
				llog("error", "Failed to decrypt %s: %v", k, err)
				continue
			}
			raw[k] = decrypted
		}
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		llog("error", "Failed to marshal settings: %v", err)
		return
	}

	// Write to ProgramData (plaintext — NTFS ACLs protect the file)
	programData := filepath.Join(os.Getenv("ProgramData"), "PunMonitor")
	os.MkdirAll(programData, 0755)
	target := filepath.Join(programData, "settings.json")
	if err := os.WriteFile(target, out, 0600); err != nil {
		llog("error", "Failed to write settings to %s: %v", target, err)
		return
	}

	// Also copy activity, audit, and election history files
	for _, name := range []string{"activity.json", "audit.jsonl", "election_history.jsonl"} {
		src := filepath.Join(userDataDir, name)
		dst := filepath.Join(programData, name)
		srcData, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		os.WriteFile(dst, srcData, 0600)
	}

	llog("info", "Settings copied to %s for service use", target)
}
