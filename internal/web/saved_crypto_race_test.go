package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSavedCryptoConcurrentAccess hammers the savedCrypto pointer + fields from
// multiple goroutines while another unlocks (swaps the pointer). With -race this
// must be clean; it deliberately exercises savedLockState/handleSavedList (which
// read s.savedCrypto without savedMu) against handleSavedUnlock (which swaps the
// pointer under savedMu) and the savedMedia.crypto pointer.
func TestSavedCryptoConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	sm, _ := newMediaDiskCache(dir+"/saved-media", 0)
	sm.crypto = sc
	s := &Server{dataDir: dir, savedCrypto: sc, savedMedia: sm}

	// Set a passphrase so the store is unlockable, then simulate a restart so
	// it comes back locked and handleSavedUnlock has real work to do.
	s.handleSavedLock(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/api/saved/lock", strings.NewReader(`{"passphrase":"pw"}`)))
	sc2, _ := loadSavedCrypto(dir)
	s.savedCrypto = sc2
	if s.savedMedia != nil {
		s.savedMedia.crypto = sc2
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: touch the pointer + fields with no savedMu.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.savedLockState(httptest.NewRecorder(),
					httptest.NewRequest("GET", "/api/saved/lock", nil))
				s.handleSavedList(httptest.NewRecorder(),
					httptest.NewRequest("GET", "/api/saved", nil))
				// Exercise the savedMedia.crypto pointer read path too.
				if s.savedMedia != nil {
					_, _, _ = s.savedMedia.Get(1, 1)
				}
			}
		}()
	}

	// Writer: repeatedly unlock (swaps s.savedCrypto + s.savedMedia.crypto).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.handleSavedUnlock(httptest.NewRecorder(),
				httptest.NewRequest("POST", "/api/saved/unlock", strings.NewReader(`{"passphrase":"pw"}`)))
		}
	}()

	time.Sleep(60 * time.Millisecond)
	close(stop)
	wg.Wait()
	_ = http.StatusOK
}
