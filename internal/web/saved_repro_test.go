package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// TestHandleSavedCreate_MediaNoHang reproduces forwarding a message that has
// media to Saved, and asserts the request returns (does not hang) and persists
// the media. Mirrors the phone repro: long-press a media post -> Forward.
func TestHandleSavedCreate_MediaNoHang(t *testing.T) {
	dir := t.TempDir()
	mc, _ := newMediaDiskCache(dir+"/mc", 0)
	sm, _ := newMediaDiskCache(dir+"/sm", 0)
	s := &Server{dataDir: dir, mediaCache: mc, savedMedia: sm}

	// handleSavedCreate now derives the domain from the active profile.
	pl := &ProfileList{Active: "p1", Profiles: []Profile{{ID: "p1", Nickname: "Test", Config: Config{Domain: "d"}}}}
	if err := s.saveProfiles(pl); err != nil {
		t.Fatal(err)
	}

	// One channel, one media message in the in-memory fallback store.
	s.channels = []protocol.ChannelInfo{{Name: "chan"}}
	size := int64(11)
	crc := uint32(0x1234abcd)
	text := "[VIDEO]11:1,0:12345:3:1234abcd\ncaption here"
	s.messages = map[int][]protocol.Message{1: {{ID: 42, Timestamp: 100, Text: text}}}

	// Media already in the ephemeral cache (as if the user had viewed it).
	body := []byte("hello world") // len 11 == size
	if err := mc.Put(size, crc, body, "video/mp4"); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest("POST", "/api/saved", strings.NewReader(`{"channelNum":1,"messageId":42}`))
		w := httptest.NewRecorder()
		s.handleSavedCreate(w, req)
		done <- w.Code
	}()

	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("status = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handleSavedCreate HUNG on a media message")
	}

	if _, _, ok := sm.Get(size, crc); !ok {
		t.Fatal("media was not persisted to saved-media")
	}
	if s.savedCount() != 1 {
		t.Fatalf("saved count = %d, want 1", s.savedCount())
	}
}
