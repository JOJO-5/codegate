package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"context"
)

func TestNewerVersion(t *testing.T) {
	for _, tc := range []struct{ next, current string; want bool }{
		{"v1.2.4", "v1.2.3", true},
		{"v1.3.0", "v1.2.99", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.2", "v1.2.3", false},
		{"dev", "v1.2.3", false},
		{"v1.2.4", "dev", false},
		{"v1.2.4-rc1", "v1.2.3", false},
	} {
		if got := newerVersion(tc.next, tc.current); got != tc.want {
			t.Errorf("newerVersion(%q,%q)=%v, want %v", tc.next, tc.current, got, tc.want)
		}
	}
}

func TestStageUpdateRequiresIdleAndValidChecksum(t *testing.T) {
	payload := []byte("test agent binary")
	sum := sha256.Sum256(payload)
	var hits atomic.Int32
	var corrupt atomic.Bool
	var interrupted atomic.Bool
	var interruptDownload atomic.Bool
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/manifest" {
			digest := hex.EncodeToString(sum[:])
			if corrupt.Load() { digest = hex.EncodeToString(make([]byte, 32)) }
			_ = json.NewEncoder(w).Encode(updateManifest{
				Version:"v1.2.4", OS:runtime.GOOS, Arch:runtime.GOARCH,
				URL:server.URL+"/binary", SHA256:digest, Size:int64(len(payload)),
			})
			return
		}
		if interruptDownload.Load() { interrupted.Store(true) }
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	oldClient := updateHTTPClient
	updateHTTPClient = server.Client()
	defer func(){ updateHTTPClient = oldClient }()
	dir := t.TempDir()
	active := func() int { return 1 }
	path, _, err := stageUpdate(context.Background(), server.URL+"/manifest", dir, "v1.2.3", active, nil)
	if err != nil || path != "" || hits.Load() != 0 {
		t.Fatalf("active session fetched update: path=%q err=%v hits=%d", path, err, hits.Load())
	}
	corrupt.Store(true)
	path, _, err = stageUpdate(context.Background(), server.URL+"/manifest", dir, "v1.2.3", func()int{return 0}, nil)
	if err == nil || path != "" {
		t.Fatalf("bad checksum accepted: path=%q err=%v", path, err)
	}
	corrupt.Store(false)
	interruptDownload.Store(true)
	path, _, err = stageUpdate(context.Background(), server.URL+"/manifest", dir, "v1.2.3", func() int {
		if interrupted.Load() { return 1 }
		return 0
	}, nil)
	if err == nil || path != "" {
		t.Fatalf("session opened during download: path=%q err=%v", path, err)
	}
	interruptDownload.Store(false)
	interrupted.Store(false)
	path, version, err := stageUpdate(context.Background(), server.URL+"/manifest", dir, "v1.2.3", func()int{return 0}, nil)
	if err != nil || path == "" || version != "v1.2.4" {
		t.Fatalf("valid artifact not staged: path=%q version=%q err=%v", path, version, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("staged bytes: %q, %v", data, err)
	}
}
