package e2e_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func backupExport(t *testing.T, base, password string, sections []string) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"password": password,
		"sections": sections,
	})
	resp, err := http.Post(base+"/api/backup/export", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/backup/export: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("export status=%d body=%s", resp.StatusCode, data)
	}
	if !bytes.HasPrefix(data, []byte("TFBAK1")) {
		t.Fatal("export does not start with TFBAK1 magic")
	}
	return data
}

func backupPreview(t *testing.T, base string, bakData []byte, password string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("password", password)
	fw, _ := mw.CreateFormFile("file", "backup.tfbak")
	fw.Write(bakData)
	mw.Close()

	resp, err := http.Post(base+"/api/backup/preview", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("POST /api/backup/preview: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("preview status=%d body=%s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	return out
}

func backupRestore(t *testing.T, base string, bakData []byte, password string, sections []string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("password", password)
	sj, _ := json.Marshal(sections)
	mw.WriteField("sections", string(sj))
	fw, _ := mw.CreateFormFile("file", "backup.tfbak")
	fw.Write(bakData)
	mw.Close()

	resp, err := http.Post(base+"/api/backup/restore", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("POST /api/backup/restore: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("restore status=%d body=%s", resp.StatusCode, body)
	}
}

// createProfile creates a profile via the profiles API and returns the profile ID.
func createProfile(t *testing.T, base, domain, key string) string {
	t.Helper()
	body := `{"action":"create","profile":{"nickname":"` + domain + `","config":{"domain":"` + domain + `","key":"` + key + `","resolvers":["127.0.0.1:9999"],"queryMode":"single","rateLimit":10}}}`
	resp := postJSON(t, base+"/api/profiles", body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create profile status=%d body=%s", resp.StatusCode, b)
	}
	// GET profiles to find the created ID
	resp2 := getJSON(t, base+"/api/profiles")
	defer resp2.Body.Close()
	var pl struct {
		Active   string `json:"active"`
		Profiles []struct {
			ID string `json:"id"`
		} `json:"profiles"`
	}
	json.NewDecoder(resp2.Body).Decode(&pl)
	if len(pl.Profiles) == 0 {
		t.Fatal("no profiles after create")
	}
	return pl.Active
}

// TestE2E_BackupExportPreview exports a backup and previews it on a fresh
// server to verify the full over-the-wire round trip.
func TestE2E_BackupExportPreview(t *testing.T) {
	base1, _ := startWebServer(t)

	// Create a profile so the backup has something
	createProfile(t, base1, "test.example.com", "pass")

	// Upload a file so we have saved data
	uploadFile(t, base1, "hello.txt", "text/plain", []byte("hello world"))

	// Export
	bakData := backupExport(t, base1, "mypassword", []string{"profiles", "saved", "savedMedia"})

	// Preview on a fresh server
	base2, _ := startWebServer(t)
	preview := backupPreview(t, base2, bakData, "mypassword")

	if preview["version"] == nil || preview["version"].(float64) != 1 {
		t.Errorf("preview version=%v, want 1", preview["version"])
	}
	if preview["profiles"] == nil {
		t.Error("preview has no profiles section")
	}
	if preview["saved"] == nil {
		t.Error("preview has no saved section")
	}
}

// TestE2E_BackupExportRestoreRoundTrip exports from server A, restores to
// server B, and verifies the restored data.
func TestE2E_BackupExportRestoreRoundTrip(t *testing.T) {
	base1, _ := startWebServer(t)

	// Setup: create profile + upload file
	createProfile(t, base1, "roundtrip.example.com", "rtp")

	content := []byte("round-trip file content")
	uploadFile(t, base1, "rt.bin", "application/octet-stream", content)

	// Export everything
	bakData := backupExport(t, base1, "secret123", []string{"profiles", "saved", "savedMedia"})

	// Restore to a fresh server
	base2, _ := startWebServer(t)
	backupRestore(t, base2, bakData, "secret123", []string{"profiles", "saved", "savedMedia"})

	// Verify profiles were restored
	resp2 := getJSON(t, base2+"/api/profiles")
	var pl struct {
		Active   string `json:"active"`
		Profiles []struct {
			ID       string `json:"id"`
			Nickname string `json:"nickname"`
		} `json:"profiles"`
	}
	json.NewDecoder(resp2.Body).Decode(&pl)
	resp2.Body.Close()
	if len(pl.Profiles) == 0 {
		t.Fatal("no profiles after restore")
	}
	if pl.Profiles[0].Nickname != "roundtrip.example.com" {
		t.Errorf("restored nickname=%v, want roundtrip.example.com", pl.Profiles[0].Nickname)
	}

	// Verify saved items were restored
	resp3 := getJSON(t, base2+"/api/saved")
	var result struct {
		Items []map[string]any `json:"items"`
	}
	json.NewDecoder(resp3.Body).Decode(&result)
	resp3.Body.Close()
	if len(result.Items) != 1 {
		t.Fatalf("restored saved items=%d, want 1", len(result.Items))
	}
	if result.Items[0]["fileName"] != "rt.bin" {
		t.Errorf("restored fileName=%v, want rt.bin", result.Items[0]["fileName"])
	}
}

// TestE2E_BackupWrongPassword verifies preview fails with wrong password.
func TestE2E_BackupWrongPassword(t *testing.T) {
	base, _ := startWebServer(t)

	createProfile(t, base, "wp.example.com", "k")

	bakData := backupExport(t, base, "correct", []string{"profiles"})

	// Preview with wrong password
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("password", "wrong")
	fw, _ := mw.CreateFormFile("file", "backup.tfbak")
	fw.Write(bakData)
	mw.Close()

	resp2, err := http.Post(base+"/api/backup/preview", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Errorf("wrong-password status=%d, want 401", resp2.StatusCode)
	}
}

// TestE2E_BackupSelectiveSections exports only profiles, then verifies the
// preview shows no saved data.
func TestE2E_BackupSelectiveSections(t *testing.T) {
	base, _ := startWebServer(t)

	createProfile(t, base, "sel.example.com", "k")

	uploadFile(t, base, "skip.txt", "text/plain", []byte("should not appear"))

	// Export only profiles
	bakData := backupExport(t, base, "pass", []string{"profiles"})

	preview := backupPreview(t, base, bakData, "pass")
	if preview["profiles"] == nil {
		t.Error("expected profiles in preview")
	}
	if preview["saved"] != nil {
		t.Error("saved should not be in profiles-only backup")
	}
}

// TestE2E_BackupNoPasswordRejected verifies export without password returns 400.
func TestE2E_BackupNoPasswordRejected(t *testing.T) {
	base, _ := startWebServer(t)

	body := `{"password":"","sections":["profiles"]}`
	resp, err := http.Post(base+"/api/backup/export", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("no-password status=%d, want 400", resp.StatusCode)
	}
}
