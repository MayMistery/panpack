package baidu

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	_, _, blockHashes, err := HashFile(filePath, 8)
	if err != nil {
		t.Fatal(err)
	}
	remoteMD5, err := RemoteMD5(blockHashes)
	if err != nil {
		t.Fatal(err)
	}

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
			writeJSON(w, map[string]any{"errno": 0, "fs_id": 1, "path": r.Form.Get("path"), "size": len(content), "md5": remoteMD5})

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
				"fs_id": 1, "path": "/apps/test/backup/test.bin", "filename": "test.bin", "size": len(content), "md5": remoteMD5,
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

func TestRemoteInfoNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/2.0/xpan/file" || r.URL.Query().Get("method") != "list" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"errno": 0, "list": []any{}})
	}))
	defer server.Close()
	client, err := NewWithAPI(api.NewClient(api.WithBaseURL(server.URL)), 4<<20, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RemoteInfo(context.Background(), "/apps/test/backup/missing.tar")
	if !errors.Is(err, ErrRemoteNotFound) {
		t.Fatalf("got %v, want ErrRemoteNotFound", err)
	}
}

func TestListDirPaginatesAndMetadataUsesHundredFileBatches(t *testing.T) {
	var mu sync.Mutex
	listCalls := 0
	metaCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Query().Get("method")
		switch method {
		case "list":
			start, _ := strconv.Atoi(r.URL.Query().Get("start"))
			mu.Lock()
			listCalls++
			mu.Unlock()
			items := make([]map[string]any, 0, 1000)
			count := 1000
			if start >= 1000 {
				count = 1
			}
			for index := 0; index < count; index++ {
				id := int64(start + index + 1)
				name := fmt.Sprintf("chunk_%04d.tar", start+index)
				items = append(items, map[string]any{"fs_id": id, "path": "/apps/test/backup/" + name, "server_filename": name, "size": id, "isdir": 0})
			}
			writeJSON(w, map[string]any{"errno": 0, "list": items})
		case "filemetas":
			var ids []int64
			if err := json.Unmarshal([]byte(r.URL.Query().Get("fsids")), &ids); err != nil {
				t.Error(err)
			}
			if len(ids) > 100 {
				t.Errorf("metadata batch has %d ids", len(ids))
			}
			mu.Lock()
			metaCalls++
			mu.Unlock()
			items := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				items = append(items, map[string]any{"fs_id": id, "path": fmt.Sprintf("/apps/test/backup/chunk_%04d.tar", id-1), "filename": fmt.Sprintf("chunk_%04d.tar", id-1), "size": id, "md5": fmt.Sprintf("md5-%d", id)})
			}
			writeJSON(w, map[string]any{"errno": 0, "list": items})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewWithAPI(api.NewClient(api.WithBaseURL(server.URL)), 4<<20, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := client.ListDir(context.Background(), "/apps/test/backup")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1001 || entries[1000].Name != "chunk_1000.tar" {
		t.Fatalf("unexpected paginated listing: count=%d last=%+v", len(entries), entries[len(entries)-1])
	}
	ids := make([]int64, 101)
	for index := range ids {
		ids[index] = int64(index + 1)
	}
	metadata, err := client.Metadata(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if listCalls != 2 || metaCalls != 2 || len(metadata) != 101 {
		t.Fatalf("unexpected calls/results: list=%d metadata=%d results=%d", listCalls, metaCalls, len(metadata))
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

func TestRemoteMD5MatchesBaiduMultipartChecksum(t *testing.T) {
	blocks := []string{
		"e2fc714c4727ee9395f324cd2e7f331f",
		"1f7690ebdd9b4caf8fab49ca1757bf27",
	}
	got, err := RemoteMD5(blocks)
	if err != nil {
		t.Fatal(err)
	}
	const want = "87b70dc7fk7fabbb2bb07077786a82eb"
	if got != want {
		t.Fatalf("remote md5=%s, want %s", got, want)
	}
	if !RemoteMD5Matches(got, "deadbeefdeadbeefdeadbeefdeadbeef", blocks) {
		t.Fatal("Baidu multipart checksum did not match its block list")
	}
	if RemoteMD5Matches(got, "deadbeefdeadbeefdeadbeefdeadbeef", blocks[:1]) {
		t.Fatal("checksum unexpectedly matched a different block list")
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
