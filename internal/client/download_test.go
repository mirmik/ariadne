package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/wire"
)

func TestDownloadQuotaAndAtomicPublication(t *testing.T) {
	for _, scenario := range []string{"exact", "over", "bad-hash", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			destination := filepath.Join(dir, "result")
			if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			serverDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverDone)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				data := []byte("abc")
				frame, _ := wire.EncodeFileData(data)
				if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
					return
				}
				if scenario == "over" {
					// Each frame is valid and small; the cumulative limit must
					// stop this even though no terminal result is ever sent.
					_ = conn.Write(ctx, websocket.MessageBinary, frame)
				} else if scenario == "cancel" {
					cancel()
				} else {
					digest := fmt.Sprintf("%x", sha256.Sum256(data))
					if scenario == "bad-hash" {
						digest = strings.Repeat("0", 64)
					}
					result, _ := wire.EncodeFileResult(wire.FileTransferResult{Size: 3, SHA256: digest})
					_ = conn.Write(ctx, websocket.MessageBinary, result)
				}
				_, _, _ = conn.Read(ctx)
			}))
			defer server.Close()
			api, err := New(Config{RelayURL: server.URL, ManagementToken: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), MaxDownloadBytes: 3})
			if err != nil {
				t.Fatal(err)
			}
			_, err = api.DownloadFile(ctx, "node", "/remote", destination, true)
			if scenario == "exact" && err != nil {
				t.Fatal(err)
			}
			if scenario != "exact" && err == nil {
				t.Fatal("expected download failure")
			}
			if scenario == "over" && !strings.Contains(err.Error(), "client size limit") {
				t.Fatalf("quota error = %v", err)
			}
			data, readErr := os.ReadFile(destination)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := []byte("original")
			if scenario == "exact" {
				want = []byte("abc")
			}
			if !bytes.Equal(data, want) {
				t.Fatalf("destination = %q, want %q", data, want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary file leaked: %v, %v", entries, err)
			}
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("download did not close stream")
			}
		})
	}
}

func TestDownloadLimitConfiguration(t *testing.T) {
	config := Config{RelayURL: "http://127.0.0.1", ManagementToken: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
	api, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if api.maxDownloadBytes != DefaultMaxDownloadBytes {
		t.Fatal("missing default quota")
	}
	config.MaxDownloadBytes = -1
	if _, err := New(config); err == nil {
		t.Fatal("negative quota accepted")
	}
}
