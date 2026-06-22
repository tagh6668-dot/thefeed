package e2e_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
)

func uploadFile(t *testing.T, base, filename, mime string, content []byte) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{`form-data; name="file"; filename="` + filename + `"`}
	if mime != "" {
		hdr["Content-Type"] = []string{mime}
	}
	pw, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatal(err)
	}
	pw.Write(content)
	mw.Close()

	resp, err := http.Post(base+"/api/saved/upload", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("POST /api/saved/upload: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("upload status=%d body=%s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return out
}

// TestE2E_UploadSmallFile uploads a small file and verifies the item metadata.
func TestE2E_UploadSmallFile(t *testing.T) {
	base, _ := startWebServer(t)

	content := []byte("hello world — small file test")
	item := uploadFile(t, base, "test.txt", "text/plain", content)

	if item["kind"] != "file" {
		t.Errorf("kind=%v, want file", item["kind"])
	}
	if item["fileName"] != "test.txt" {
		t.Errorf("fileName=%v, want test.txt", item["fileName"])
	}
	media, ok := item["media"].([]any)
	if !ok || len(media) == 0 {
		t.Fatalf("no media in response: %v", item["media"])
	}
	md := media[0].(map[string]any)
	if md["persisted"] != true {
		t.Errorf("persisted=%v, want true", md["persisted"])
	}
}

// TestE2E_UploadAndDownload uploads a file and fetches it back via saved-media.
func TestE2E_UploadAndDownload(t *testing.T) {
	base, _ := startWebServer(t)

	content := bytes.Repeat([]byte{0xAB}, 1024)
	item := uploadFile(t, base, "data.bin", "application/octet-stream", content)

	media := item["media"].([]any)
	md := media[0].(map[string]any)
	size := int64(md["size"].(float64))
	crc := uint32(md["crc"].(float64))
	crcHex := fmt.Sprintf("%x", crc)

	url := fmt.Sprintf("%s/api/saved/media?size=%d&crc=%s", base, size, crcHex)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET saved media: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(content))
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/octet-stream" {
		t.Errorf("Content-Type=%q, want application/octet-stream", ct)
	}
}

// TestE2E_UploadLargeFile uploads a file larger than 8 MiB (the former
// ParseMultipartForm threshold) to verify the fix for temp-file failures.
func TestE2E_UploadLargeFile(t *testing.T) {
	base, _ := startWebServer(t)

	// 10 MiB random payload — above old 8 MiB maxMemory threshold.
	content := make([]byte, 10<<20)
	rand.Read(content)
	item := uploadFile(t, base, "big.bin", "application/octet-stream", content)

	media := item["media"].([]any)
	md := media[0].(map[string]any)
	size := int64(md["size"].(float64))
	crc := uint32(md["crc"].(float64))
	crcHex := fmt.Sprintf("%x", crc)

	// Verify download matches
	url := fmt.Sprintf("%s/api/saved/media?size=%d&crc=%s", base, size, crcHex)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET saved media: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(content))
	}
}

// TestE2E_UploadOverLimit verifies a file exceeding the 50 MiB cap is rejected.
func TestE2E_UploadOverLimit(t *testing.T) {
	base, _ := startWebServer(t)

	// 50 MiB + 2 KiB — just over the limit
	big := make([]byte, 50<<20+2048)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{`form-data; name="file"; filename="huge.bin"`}
	hdr["Content-Type"] = []string{"application/octet-stream"}
	pw, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatal(err)
	}
	pw.Write(big)
	mw.Close()

	resp, err := http.Post(base+"/api/saved/upload", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Errorf("status=%d, want 413", resp.StatusCode)
	}
}

// TestE2E_UploadAppearsInSavedList verifies an uploaded file shows up in the
// saved list and can be retrieved by GET /api/saved.
func TestE2E_UploadAppearsInSavedList(t *testing.T) {
	base, _ := startWebServer(t)

	content := []byte("list-check-content")
	uploadFile(t, base, "check.txt", "text/plain", content)

	resp := getJSON(t, base+"/api/saved")
	defer resp.Body.Close()
	var result struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode saved list: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("saved list has %d items, want 1", len(result.Items))
	}
	if result.Items[0]["fileName"] != "check.txt" {
		t.Errorf("fileName=%v, want check.txt", result.Items[0]["fileName"])
	}
}
