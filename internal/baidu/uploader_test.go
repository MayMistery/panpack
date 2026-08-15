package baidu

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/baidu-netdisk/baidu-drive-sdk-go/baidudriver/api"
)

func TestUploadFileUsesOfficialThreeStepProtocolAndVerifies(t *testing.T) {
	content := []byte("abcdefghijklmnopqrstuvwxyz")
	filePath := filepath.Join(t.TempDir(), "test.bin")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	full := md5.Sum(content)
	fullMD5 := hex.EncodeToString(full[:])

	var mu sync.Mutex
	parts := map[int][]byte{}
	metaCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Query().Get("method")
		switch {
		case r.URL.Path == "/rest/2.0/xpan/file" && method == "precreate":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			var hashes []string
			if err := json.Unmarshal([]byte(r.Form.Get("block_list")), &hashes); err != nil {
				t.Error(err)
			}
			missing := make([]int, len(hashes))
			for i := range missing {
				missing[i] = i
			}
			writeJSON(w, map[string]any{"errno": 0, "uploadid": "upload-1", "block_list": missing, "return_type": 1})

		case r.URL.Path == "/rest/2.0/pcs/superfile2" && method == "upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(file)
			_ = file.Close()
			index, _ := strconv.Atoi(r.URL.Query().Get("partseq"))
			mu.Lock()
			parts[index] = append([]byte(nil), data...)
			mu.Unlock()
			h := md5.Sum(data)
			writeJSON(w, map[string]any{"errno": 0, "md5": hex.EncodeToString(h[:])})

		case r.URL.Path == "/rest/2.0/xpan/file" && method == "create":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			writeJSON(w, map[string]any{"errno": 0, "fs_id": 1, "path": r.Form.Get("path"), "size": len(content), "md5": fullMD5})

		case r.URL.Path == "/rest/2.0/xpan/file" && method == "list":
			writeJSON(w, map[string]any{"errno": 0, "list": []map[string]any{{
				"fs_id": 1, "path": "/apps/test/backup/test.bin", "server_filename": "test.bin", "size": len(content),
			}}})

		case r.URL.Path == "/rest/2.0/xpan/multimedia" && method == "filemetas":
			if got := r.URL.Query().Get("fsids"); got != "[1]" {
				t.Errorf("unexpected fsids: %s", got)
			}
			mu.Lock()
			metaCalled = true
			mu.Unlock()
			writeJSON(w, map[string]any{"errno": 0, "list": []map[string]any{{
				"fs_id": 1, "path": "/apps/test/backup/test.bin", "filename": "test.bin", "size": len(content), "md5": fullMD5,
			}}})

		default:
			http.Error(w, fmt.Sprintf("unexpected request %s %s", r.Method, r.URL.String()), http.StatusNotFound)
		}
	}))
	defer server.Close()

	apiClient := api.NewClient(api.WithBaseURL(server.URL), api.WithPCSBaseURL(server.URL))
	client, err := NewWithAPI(apiClient, 8, 2, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := client.UploadFile(context.Background(), filePath, "/apps/test/backup/test.bin")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Concurrency != 2 || client.CurrentConcurrency() != 3 {
		t.Fatalf("unexpected adaptive concurrency: stats=%d current=%d", stats.Concurrency, client.CurrentConcurrency())
	}

	mu.Lock()
	if !metaCalled {
		mu.Unlock()
		t.Fatal("filemetas was not called")
	}
	indexes := make([]int, 0, len(parts))
	for index := range parts {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	var uploaded []byte
	for _, index := range indexes {
		uploaded = append(uploaded, parts[index]...)
	}
	mu.Unlock()
	if string(uploaded) != string(content) {
		t.Fatalf("uploaded bytes differ: %q", uploaded)
	}
}

func TestValidateRemotePath(t *testing.T) {
	for _, invalid := range []string{"relative", "/tmp/file", "/apps"} {
		if _, err := validateRemotePath(invalid); err == nil {
			t.Errorf("validateRemotePath(%q) unexpectedly succeeded", invalid)
		}
	}
	if got, err := validateRemotePath("/apps/test/a/../b"); err != nil || got != "/apps/test/b" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestAdaptiveConcurrencyFeedback(t *testing.T) {
	client, err := NewWithAPI(api.NewClient(), 4<<20, 8, 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.feedback(UploadStats{Concurrency: 8, Retries: 4})
	if got := client.CurrentConcurrency(); got != 4 {
		t.Fatalf("after retry pressure concurrency=%d, want 4", got)
	}
	client.feedback(UploadStats{Concurrency: 4})
	if got := client.CurrentConcurrency(); got != 5 {
		t.Fatalf("after clean volume concurrency=%d, want 5", got)
	}
	client.feedback(UploadStats{Concurrency: 5, RateLimits: 1})
	if got := client.CurrentConcurrency(); got != 2 {
		t.Fatalf("after rate limit concurrency=%d, want 2", got)
	}
}

func TestHashEmptyFileHasRequiredBlock(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(filePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	size, full, blocks, err := HashFile(filePath, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 || full != "d41d8cd98f00b204e9800998ecf8427e" || len(blocks) != 1 || blocks[0] != full {
		t.Fatalf("unexpected empty hash result: size=%d full=%s blocks=%v", size, full, blocks)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
