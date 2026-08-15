package baidu

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MayMistery/panpack/internal/state"
	"github.com/baidu-netdisk/baidu-drive-sdk-go/baidudriver/api"
)

const defaultMaxRetries = 5

var ErrRemoteNotFound = errors.New("remote file not found")

type Client struct {
	api            *api.Client
	blockSize      int64
	concurrency    int
	maxConcurrency int
	maxRetries     int
	logger         *log.Logger
	mu             sync.Mutex
}

type UploadStats struct {
	Concurrency int
	Retries     int
	RateLimits  int
	Duration    time.Duration
	Rapid       bool
	Size        int64
	MD5         string
}

type RemoteInfo struct {
	FsID int64
	Path string
	Size int64
	MD5  string
}

func New(accessToken string, blockSize int64, initialConcurrency, maxConcurrency int, logger *log.Logger) (*Client, error) {
	if accessToken == "" {
		return nil, errors.New("access token is empty")
	}
	return NewWithAPI(api.NewClient(api.WithAccessToken(accessToken)), blockSize, initialConcurrency, maxConcurrency, logger)
}

func NewWithAPI(apiClient *api.Client, blockSize int64, initialConcurrency, maxConcurrency int, logger *log.Logger) (*Client, error) {
	if apiClient == nil {
		return nil, errors.New("API client is nil")
	}
	if blockSize <= 0 {
		return nil, errors.New("block size must be positive")
	}
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	if initialConcurrency < 1 {
		initialConcurrency = 1
	}
	if initialConcurrency > maxConcurrency {
		initialConcurrency = maxConcurrency
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Client{
		api: apiClient, blockSize: blockSize,
		concurrency: initialConcurrency, maxConcurrency: maxConcurrency,
		maxRetries: defaultMaxRetries, logger: logger,
	}, nil
}

func (c *Client) EnsureDir(ctx context.Context, remoteDir string) error {
	clean, err := validateRemotePath(remoteDir)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) < 3 {
		return nil // /apps/<app> is provisioned by Baidu
	}
	current := "/" + path.Join(parts[0], parts[1])
	for _, part := range parts[2:] {
		current = path.Join(current, part)
		_, err := c.api.FileManager.Mkdir(ctx, &api.MkdirParams{Path: current})
		if err == nil || api.IsErrno(err, api.ErrnoFileAlreadyExist) {
			continue
		}
		return fmt.Errorf("create remote directory %s: %w", current, err)
	}
	return nil
}

func (c *Client) UploadVolume(ctx context.Context, volume state.Volume) (UploadStats, error) {
	return c.upload(ctx, volume.LocalPath, volume.RemotePath, volume.Size, volume.MD5, volume.BlockMD5s)
}

func (c *Client) UploadFile(ctx context.Context, localPath, remotePath string) (UploadStats, error) {
	size, fullMD5, blocks, err := HashFile(localPath, c.blockSize)
	if err != nil {
		return UploadStats{}, err
	}
	return c.UploadHashedFile(ctx, localPath, remotePath, size, fullMD5, blocks)
}

// UploadHashedFile uploads a file whose whole-file and API block hashes have
// already been computed. The local size and every block hash are rechecked as
// the bytes are read, so callers can safely use this to avoid hashing twice.
func (c *Client) UploadHashedFile(ctx context.Context, localPath, remotePath string, size int64, fullMD5 string, blocks []string) (UploadStats, error) {
	return c.upload(ctx, localPath, remotePath, size, fullMD5, blocks)
}

func (c *Client) upload(ctx context.Context, localPath, remotePath string, size int64, fullMD5 string, blocks []string) (UploadStats, error) {
	if _, err := validateRemotePath(remotePath); err != nil {
		return UploadStats{}, err
	}
	if len(blocks) == 0 {
		return UploadStats{}, errors.New("file has no block hashes")
	}
	fi, err := os.Stat(localPath)
	if err != nil {
		return UploadStats{}, err
	}
	if fi.Size() != size {
		return UploadStats{}, fmt.Errorf("local file size changed: %d != %d", fi.Size(), size)
	}

	c.mu.Lock()
	concurrency := c.concurrency
	c.mu.Unlock()
	stats := UploadStats{Concurrency: concurrency, Size: size, MD5: fullMD5}
	started := time.Now()
	rtype := 2 // overwrite makes retries idempotent
	var pre *api.PrecreateResponse
	retries, rates, err := c.retry(ctx, func() error {
		var callErr error
		pre, callErr = c.api.Upload.Precreate(ctx, &api.PrecreateParams{
			Path: remotePath, Size: size, BlockList: blocks, RType: &rtype,
		})
		return callErr
	})
	stats.Retries += retries
	stats.RateLimits += rates
	if err != nil {
		c.feedback(stats)
		return stats, fmt.Errorf("precreate %s: %w", remotePath, err)
	}
	if pre.ReturnType == 2 {
		stats.Rapid = true
		if err := c.Verify(ctx, remotePath, size, fullMD5, blocks); err != nil {
			c.feedback(stats)
			return stats, fmt.Errorf("verify rapid upload: %w", err)
		}
		stats.Duration = time.Since(started)
		c.feedback(stats)
		return stats, nil
	}
	if pre.UploadID == "" {
		return stats, errors.New("precreate returned no uploadid")
	}

	missing := append([]int(nil), pre.BlockList...)
	sort.Ints(missing)
	for _, index := range missing {
		if index < 0 || index >= len(blocks) {
			return stats, fmt.Errorf("precreate requested invalid block %d of %d", index, len(blocks))
		}
	}
	retryCount, rateCount, err := c.uploadBlocks(ctx, localPath, remotePath, pre.UploadID, size, blocks, missing, concurrency)
	stats.Retries += retryCount
	stats.RateLimits += rateCount
	if err != nil {
		stats.Duration = time.Since(started)
		c.feedback(stats)
		return stats, err
	}

	var created *api.CreateFileResponse
	retries, rates, err = c.retry(ctx, func() error {
		var callErr error
		created, callErr = c.api.Upload.CreateFile(ctx, &api.CreateFileParams{
			Path: remotePath, Size: size, UploadID: pre.UploadID, BlockList: blocks, RType: &rtype,
		})
		return callErr
	})
	stats.Retries += retries
	stats.RateLimits += rates
	stats.Duration = time.Since(started)
	if err != nil {
		c.feedback(stats)
		return stats, fmt.Errorf("create remote file %s: %w", remotePath, err)
	}
	if created.Size != size {
		c.feedback(stats)
		return stats, fmt.Errorf("remote size mismatch: got %d, want %d", created.Size, size)
	}
	if created.MD5 != "" && !RemoteMD5Matches(created.MD5, fullMD5, blocks) {
		c.feedback(stats)
		return stats, fmt.Errorf("remote md5 mismatch: got %s, want whole-file or Baidu composite checksum", created.MD5)
	}
	if err := c.Verify(ctx, remotePath, size, fullMD5, blocks); err != nil {
		c.feedback(stats)
		return stats, err
	}
	c.feedback(stats)
	return stats, nil
}

func (c *Client) uploadBlocks(ctx context.Context, localPath, remotePath, uploadID string, fileSize int64, expected []string, missing []int, concurrency int) (int, int, error) {
	if len(missing) == 0 {
		return 0, 0, nil
	}
	if concurrency > len(missing) {
		concurrency = len(missing)
	}
	file, err := os.Open(localPath)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	var retries atomic.Int64
	var rateLimits atomic.Int64
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if workCtx.Err() != nil {
					return
				}
				r, rate, err := c.uploadBlock(workCtx, file, remotePath, uploadID, fileSize, index, expected[index])
				retries.Add(int64(r))
				rateLimits.Add(int64(rate))
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}
	for _, index := range missing {
		select {
		case jobs <- index:
		case <-workCtx.Done():
			break
		}
		if workCtx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errCh:
		return int(retries.Load()), int(rateLimits.Load()), err
	default:
		return int(retries.Load()), int(rateLimits.Load()), ctx.Err()
	}
}

func (c *Client) uploadBlock(ctx context.Context, file *os.File, remotePath, uploadID string, fileSize int64, index int, expectedMD5 string) (int, int, error) {
	offset := int64(index) * c.blockSize
	size := c.blockSize
	if offset+size > fileSize {
		size = fileSize - offset
	}
	if size < 0 || (size == 0 && !(fileSize == 0 && index == 0)) {
		return 0, 0, fmt.Errorf("invalid block %d range", index)
	}
	buf := make([]byte, size)
	if len(buf) > 0 {
		if _, err := file.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
			return 0, 0, fmt.Errorf("read block %d: %w", index, err)
		}
	}
	h := md5.Sum(buf)
	if !strings.EqualFold(hex.EncodeToString(h[:]), expectedMD5) {
		return 0, 0, fmt.Errorf("local block %d md5 changed", index)
	}
	var response *api.SliceUploadResponse
	retries, rates, err := c.retry(ctx, func() error {
		var callErr error
		response, callErr = c.api.Upload.SliceUpload(ctx, &api.SliceUploadParams{
			Path: remotePath, UploadID: uploadID, PartSeq: index, File: bytes.NewReader(buf),
		})
		if callErr == nil && !strings.EqualFold(response.MD5, expectedMD5) {
			callErr = fmt.Errorf("server returned md5 %s for block %d, expected %s", response.MD5, index, expectedMD5)
		}
		return callErr
	})
	if err != nil {
		return retries, rates, fmt.Errorf("upload block %d: %w", index, err)
	}
	return retries, rates, nil
}

func (c *Client) RemoteInfo(ctx context.Context, remotePath string) (RemoteInfo, error) {
	remotePath, err := validateRemotePath(remotePath)
	if err != nil {
		return RemoteInfo{}, err
	}
	dir, name := path.Split(remotePath)
	dir = strings.TrimSuffix(dir, "/")
	start, limit := 0, 1000
	order, desc := "name", 0
	for {
		listing, err := c.api.File.List(ctx, &api.ListParams{Dir: dir, Order: &order, Desc: &desc, Start: &start, Limit: &limit})
		if err != nil {
			return RemoteInfo{}, fmt.Errorf("list remote directory %s: %w", dir, err)
		}
		for _, file := range listing.List {
			if file.ServerFilename != name && file.Path != remotePath {
				continue
			}
			// The list API does not reliably include an MD5. Resolve the file's
			// fs_id first, then use filemetas, whose contract includes size and MD5.
			metadata, err := c.api.Download.Meta(ctx, &api.MetaParams{FsIDs: []int64{file.FsID}})
			if err != nil {
				return RemoteInfo{}, fmt.Errorf("read remote metadata for %s: %w", remotePath, err)
			}
			for _, item := range metadata.List {
				if item.FsID != file.FsID {
					continue
				}
				if item.MD5 == "" {
					return RemoteInfo{}, fmt.Errorf("remote metadata for %s has no md5", remotePath)
				}
				return RemoteInfo{FsID: item.FsID, Path: item.Path, Size: item.Size, MD5: item.MD5}, nil
			}
			return RemoteInfo{}, fmt.Errorf("remote metadata not found for %s (fs_id=%d)", remotePath, file.FsID)
		}
		if len(listing.List) < limit {
			return RemoteInfo{}, fmt.Errorf("%w: %s", ErrRemoteNotFound, remotePath)
		}
		start += len(listing.List)
	}
}

func (c *Client) Verify(ctx context.Context, remotePath string, size int64, expectedMD5 string, blocks []string) error {
	info, err := c.RemoteInfo(ctx, remotePath)
	if err != nil {
		return err
	}
	if info.Size != size {
		return fmt.Errorf("remote size mismatch for %s: %d != %d", remotePath, info.Size, size)
	}
	if expectedMD5 != "" && !RemoteMD5Matches(info.MD5, expectedMD5, blocks) {
		return fmt.Errorf("remote md5 mismatch for %s: %s does not match whole-file or Baidu composite checksum", remotePath, info.MD5)
	}
	return nil
}

// RemoteMD5Matches accepts both API variants observed in practice: a plain
// whole-file MD5 and Baidu's encoded checksum of the compact JSON block list.
// The latter is what the create, list, and filemetas APIs return for multipart
// files. Since every uploaded block is checked separately, the composite binds
// the verified bytes and their order without downloading the remote file again.
func RemoteMD5Matches(remote, fullMD5 string, blocks []string) bool {
	if strings.EqualFold(remote, fullMD5) {
		return true
	}
	composite, err := RemoteMD5(blocks)
	return err == nil && strings.EqualFold(remote, composite)
}

// RemoteMD5 calculates Baidu's multipart metadata checksum.
func RemoteMD5(blocks []string) (string, error) {
	if len(blocks) == 0 {
		return "", errors.New("cannot calculate remote md5 without blocks")
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return "", fmt.Errorf("marshal block list: %w", err)
	}
	sum := md5.Sum(encoded)
	return encodeBaiduMD5(hex.EncodeToString(sum[:]))
}

func encodeBaiduMD5(value string) (string, error) {
	if len(value) != md5.Size*2 {
		return "", fmt.Errorf("invalid md5 length %d", len(value))
	}
	reordered := value[8:16] + value[0:8] + value[24:32] + value[16:24]
	encoded := make([]byte, len(reordered))
	for i := range reordered {
		nibble, ok := hexNibble(reordered[i])
		if !ok {
			return "", fmt.Errorf("invalid md5 character %q", reordered[i])
		}
		encoded[i] = "0123456789abcdef"[nibble^byte(i&15)]
	}
	ninth, _ := hexNibble(encoded[9])
	encoded[9] = 'g' + ninth
	return string(encoded), nil
}

func hexNibble(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func (c *Client) retry(ctx context.Context, fn func() error) (retries, rateLimits int, finalErr error) {
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return retries, rateLimits, err
		}
		err := fn()
		if err == nil {
			return retries, rateLimits, nil
		}
		finalErr = err
		if api.IsErrno(err, api.ErrnoLimitExceeded) {
			rateLimits++
		}
		if attempt+1 == c.maxRetries {
			break
		}
		retries++
		wait := time.Second << attempt
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		c.logger.Printf("request failed; retry %d/%d in %s: %v", attempt+1, c.maxRetries-1, wait, err)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return retries, rateLimits, ctx.Err()
		case <-timer.C:
		}
	}
	return retries, rateLimits, finalErr
}

func (c *Client) feedback(stats UploadStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.concurrency
	if stats.RateLimits > 0 || stats.Retries >= maxInt(2, stats.Concurrency/2) {
		c.concurrency /= 2
		if c.concurrency < 1 {
			c.concurrency = 1
		}
	} else if stats.Retries == 0 && c.concurrency < c.maxConcurrency {
		c.concurrency++
	}
	if old != c.concurrency {
		c.logger.Printf("adaptive upload concurrency: %d -> %d", old, c.concurrency)
	}
}

func (c *Client) CurrentConcurrency() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.concurrency
}

func HashFile(filePath string, blockSize int64) (size int64, fullMD5 string, blocks []string, err error) {
	if blockSize <= 0 {
		return 0, "", nil, errors.New("block size must be positive")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return 0, "", nil, err
	}
	defer f.Close()
	full := md5.New()
	buf := make([]byte, blockSize)
	for {
		n, readErr := io.ReadFull(f, buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = full.Write(chunk)
			h := md5.Sum(chunk)
			blocks = append(blocks, hex.EncodeToString(h[:]))
			size += int64(n)
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return 0, "", nil, readErr
		}
	}
	if len(blocks) == 0 { // the API still requires a block_list for an empty file
		h := md5.Sum(nil)
		blocks = append(blocks, hex.EncodeToString(h[:]))
	}
	return size, hex.EncodeToString(full.Sum(nil)), blocks, nil
}

func validateRemotePath(remotePath string) (string, error) {
	if remotePath == "" || !strings.HasPrefix(remotePath, "/") {
		return "", fmt.Errorf("remote path must be absolute: %q", remotePath)
	}
	clean := path.Clean(remotePath)
	if !strings.HasPrefix(clean, "/apps/") || len(strings.Split(strings.TrimPrefix(clean, "/"), "/")) < 2 {
		return "", fmt.Errorf("remote path must be under /apps/<app>: %q", remotePath)
	}
	return clean, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
