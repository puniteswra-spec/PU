// push_release: Decrypts the DPAPI-encrypted GitHub token from settings.json
// and pushes a new release (with attached binary) to the configured repo.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	crypt32                = syscall.NewLazyDLL("Crypt32.dll")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32               = syscall.NewLazyDLL("Kernel32.dll")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func dpapiDecrypt(ciphertext []byte) ([]byte, error) {
	var in, out dataBlob
	in.cbData = uint32(len(ciphertext))
	in.pbData = &ciphertext[0]
	r, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		0,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CryptUnprotectData failed: %v", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	plain := make([]byte, out.cbData)
	for i := uint32(0); i < out.cbData; i++ {
		plain[i] = *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(out.pbData)) + uintptr(i)))
	}
	return plain, nil
}

type Settings struct {
	GitHubRepo    string `json:"github_repo"`
	GitHubTokenEnc string `json:"github_token"`
}

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: push_release <settings.json> <binary-path> <version>")
		os.Exit(1)
	}
	settingsPath := os.Args[1]
	binPath := os.Args[2]
	version := os.Args[3]

	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Println("read settings:", err)
		os.Exit(1)
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		fmt.Println("parse settings:", err)
		os.Exit(1)
	}
	if !strings.HasPrefix(s.GitHubTokenEnc, "enc:dpapi:") {
		fmt.Println("token is not DPAPI-encrypted")
		os.Exit(1)
	}
	cipher, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s.GitHubTokenEnc, "enc:dpapi:"))
	if err != nil {
		fmt.Println("base64 decode:", err)
		os.Exit(1)
	}
	token, err := dpapiDecrypt(cipher)
	if err != nil {
		fmt.Println("dpapi decrypt:", err)
		os.Exit(1)
	}
	fmt.Println("Decrypted token (first 8):", string(token[:8])+"...")
	fmt.Println("Repo:", s.GitHubRepo)

	tag := "v" + version
	fmt.Println("\n=== Step 1: Create release", tag, "===")
	createURL := "https://api.github.com/repos/" + s.GitHubRepo + "/releases"
	body, _ := json.Marshal(map[string]interface{}{
		"tag_name":         tag,
		"target_commitish": "main",
		"name":             "PunMonitor " + tag,
		"body":             "PunMonitor v" + version + " - Focus fix (single-mode display), ESC-to-exit-focus, quality overlay, Picture-in-Picture mode",
		"draft":            false,
		"prerelease":       false,
	})
	req, _ := http.NewRequest("POST", createURL, bytes.NewReader(body))
	req.Header.Set("Authorization", "token "+string(token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("create release:", err)
		os.Exit(1)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		fmt.Println("create release failed:", resp.StatusCode)
		fmt.Println(string(respBody))
		os.Exit(1)
	}
	var rel struct {
		ID      int64 `json:"id"`
		UploadURL string `json:"upload_url"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &rel); err != nil {
		fmt.Println("parse response:", err)
		os.Exit(1)
	}
	fmt.Println("Release created:", rel.HTMLURL)
	fmt.Println("Upload URL:", rel.UploadURL)

	fmt.Println("\n=== Step 2: Upload binary ===")
	binData, err := os.ReadFile(binPath)
	if err != nil {
		fmt.Println("read binary:", err)
		os.Exit(1)
	}
	uploadURL := rel.UploadURL
	uploadURL = strings.SplitN(uploadURL, "{", 2)[0]
	uploadURL = uploadURL + "?name=PunMonitor.exe"
	req, _ = http.NewRequest("POST", uploadURL, bytes.NewReader(binData))
	req.Header.Set("Authorization", "token "+string(token))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("upload:", err)
		os.Exit(1)
	}
	upBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		fmt.Println("upload failed:", resp.StatusCode)
		fmt.Println(string(upBody))
		os.Exit(1)
	}
	var asset struct {
		BrowserDownloadURL string `json:"browser_download_url"`
		Name               string `json:"name"`
		Size               int    `json:"size"`
	}
	if err := json.Unmarshal(upBody, &asset); err != nil {
		fmt.Println("parse upload response:", err)
		os.Exit(1)
	}
	fmt.Println("Uploaded:", asset.Name, "(", asset.Size, "bytes )")
	fmt.Println("Download URL:", asset.BrowserDownloadURL)
	fmt.Println("\n=== DONE ===")
	fmt.Println("Release:", rel.HTMLURL)
	fmt.Println("Binary: ", asset.BrowserDownloadURL)
}
