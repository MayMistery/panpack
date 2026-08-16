package audit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MayMistery/panpack/internal/baidu"
	"github.com/MayMistery/panpack/internal/batchupload"
	"github.com/MayMistery/panpack/internal/credentials"
	"github.com/MayMistery/panpack/internal/runreceipt"
)

type Config struct {
	StateFile         string
	TokenFile         string
	RemotePattern     string
	ExpectedNames     []string
	RequireLocalEmpty bool
	RequireChecksum   bool
	ReceiptFile       string
	Logger            *log.Logger
}

type SizeMismatch struct {
	Name     string `json:"name"`
	Expected int64  `json:"expected"`
	Actual   int64  `json:"actual"`
}

type ReceiptEvidence struct {
	Checked  bool              `json:"checked"`
	Status   runreceipt.Status `json:"status,omitempty"`
	ExitCode *int              `json:"exit_code,omitempty"`
	Error    string            `json:"error,omitempty"`
}

type Result struct {
	Passed                    bool            `json:"passed"`
	StateCompleted            bool            `json:"state_completed"`
	StateUploaded             int             `json:"state_uploaded"`
	StateFiles                int             `json:"state_files"`
	StateSHA256               string          `json:"state_sha256"`
	VerifiedBytes             int64           `json:"verified_bytes"`
	TotalBytes                int64           `json:"total_bytes"`
	RemotePattern             string          `json:"remote_pattern"`
	RemoteEntries             int             `json:"remote_entries"`
	RemoteUnique              int             `json:"remote_unique"`
	ExpectedEntries           int             `json:"expected_entries"`
	Missing                   []string        `json:"missing,omitempty"`
	Extra                     []string        `json:"extra,omitempty"`
	Duplicates                []string        `json:"duplicates,omitempty"`
	TypeMismatches            []string        `json:"type_mismatches,omitempty"`
	FrozenNotExpected         []string        `json:"frozen_not_expected,omitempty"`
	FrozenSizeMatched         int             `json:"frozen_size_matched"`
	FrozenSizeMismatches      []SizeMismatch  `json:"frozen_size_mismatches,omitempty"`
	FrozenMetadataMissing     []string        `json:"frozen_metadata_missing,omitempty"`
	FrozenChecksumMatched     int             `json:"frozen_checksum_matched"`
	FrozenChecksumUnavailable []string        `json:"frozen_checksum_unavailable,omitempty"`
	FrozenChecksumMismatches  []string        `json:"frozen_checksum_mismatches,omitempty"`
	LocalMatching             []string        `json:"local_matching,omitempty"`
	Receipt                   ReceiptEvidence `json:"receipt"`
	Issues                    []string        `json:"issues,omitempty"`
}

type remoteClient interface {
	ListDir(context.Context, string) ([]baidu.RemoteInfo, error)
	Metadata(context.Context, []int64) (map[int64]baidu.RemoteInfo, error)
}

func Run(ctx context.Context, cfg Config) (*Result, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	creds, _, err := credentials.Discover(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	client, err := baidu.New(creds.AccessToken, 4<<20, 1, 1, logger)
	if err != nil {
		return nil, err
	}
	return RunWithClient(ctx, cfg, client)
}

func RunWithClient(ctx context.Context, cfg Config, client remoteClient) (*Result, error) {
	if cfg.StateFile == "" {
		return nil, errors.New("state file is required")
	}
	stateFile, err := filepath.Abs(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	if cfg.ReceiptFile != "" {
		cfg.ReceiptFile, err = filepath.Abs(cfg.ReceiptFile)
		if err != nil {
			return nil, err
		}
	}
	stateSHA256, err := runreceipt.FileSHA256(stateFile)
	if err != nil {
		return nil, fmt.Errorf("hash upload state before audit: %w", err)
	}
	state, err := batchupload.LoadState(stateFile)
	if err != nil {
		return nil, err
	}
	result := &Result{
		StateCompleted: state.Completed,
		StateFiles:     len(state.Files),
		StateSHA256:    stateSHA256,
		TotalBytes:     state.TotalBytes,
	}
	if !state.Completed {
		result.Issues = append(result.Issues, "upload state is not complete")
	}

	stateNames := make(map[string]struct{}, len(state.Files))
	var recordedBytes int64
	for _, record := range state.Files {
		if record.Name == "" || path.Base(record.Name) != record.Name {
			result.Issues = append(result.Issues, fmt.Sprintf("unsafe filename in upload state: %q", record.Name))
			continue
		}
		if _, exists := stateNames[record.Name]; exists {
			result.Issues = append(result.Issues, fmt.Sprintf("duplicate filename in upload state: %s", record.Name))
			continue
		}
		stateNames[record.Name] = struct{}{}
		recordedBytes += record.Size
		if record.Uploaded {
			result.StateUploaded++
			result.VerifiedBytes += record.Size
			if record.MD5 == "" {
				result.FrozenChecksumUnavailable = append(result.FrozenChecksumUnavailable, record.Name)
				result.Issues = append(result.Issues, fmt.Sprintf("uploaded state file %s has no verified checksum", record.Name))
			}
		}
	}
	if result.StateUploaded != result.StateFiles {
		result.Issues = append(result.Issues, fmt.Sprintf("upload state has %d/%d uploaded files", result.StateUploaded, result.StateFiles))
	}
	if recordedBytes != state.TotalBytes {
		result.Issues = append(result.Issues, fmt.Sprintf("upload state byte total is %d, records total %d", state.TotalBytes, recordedBytes))
	}

	pattern := cfg.RemotePattern
	if pattern == "" {
		pattern = state.Pattern
	}
	if pattern == "" || path.Base(pattern) != pattern {
		return result, fmt.Errorf("remote pattern must match basenames only: %q", pattern)
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return result, fmt.Errorf("invalid remote pattern %q: %w", pattern, err)
	}
	result.RemotePattern = pattern

	expected, err := normalizeExpected(cfg.ExpectedNames, state)
	if err != nil {
		return result, err
	}
	for name := range expected {
		matched, matchErr := path.Match(pattern, name)
		if matchErr != nil {
			return result, matchErr
		}
		if !matched {
			return result, fmt.Errorf("expected name %q does not match remote pattern %q", name, pattern)
		}
	}
	result.ExpectedEntries = len(expected)
	for name := range stateNames {
		if _, ok := expected[name]; !ok {
			result.FrozenNotExpected = append(result.FrozenNotExpected, name)
		}
	}
	sort.Strings(result.FrozenNotExpected)
	if len(result.FrozenNotExpected) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("%d frozen files are outside the expected remote set", len(result.FrozenNotExpected)))
	}

	entries, err := client.ListDir(ctx, state.RemoteDir)
	if err != nil {
		return result, err
	}
	remote := make(map[string]baidu.RemoteInfo, len(entries))
	for _, entry := range entries {
		name := entry.Name
		if name == "" {
			name = path.Base(entry.Path)
		}
		matched, matchErr := path.Match(pattern, name)
		if matchErr != nil {
			return result, matchErr
		}
		if !matched {
			continue
		}
		result.RemoteEntries++
		if _, exists := remote[name]; exists {
			result.Duplicates = append(result.Duplicates, name)
			continue
		}
		remote[name] = entry
	}
	result.RemoteUnique = len(remote)
	sort.Strings(result.Duplicates)
	for name := range expected {
		entry, ok := remote[name]
		if !ok {
			result.Missing = append(result.Missing, name)
			continue
		}
		if entry.IsDir {
			result.TypeMismatches = append(result.TypeMismatches, name)
		}
	}
	for name := range remote {
		if _, ok := expected[name]; !ok {
			result.Extra = append(result.Extra, name)
		}
	}
	sort.Strings(result.Missing)
	sort.Strings(result.Extra)
	sort.Strings(result.TypeMismatches)
	if len(result.Missing) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("remote set is missing %d expected files", len(result.Missing)))
	}
	if len(result.Extra) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("remote set has %d unexpected files", len(result.Extra)))
	}
	if len(result.Duplicates) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("remote set has %d duplicate names", len(result.Duplicates)))
	}
	if len(result.TypeMismatches) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("%d expected remote files are directories", len(result.TypeMismatches)))
	}

	metadataIDs := make([]int64, 0, len(state.Files))
	for _, record := range state.Files {
		entry, ok := remote[record.Name]
		if !ok || entry.IsDir {
			continue
		}
		if record.MD5 != "" {
			metadataIDs = append(metadataIDs, entry.FsID)
		}
	}

	metadata := map[int64]baidu.RemoteInfo{}
	if len(metadataIDs) > 0 {
		metadata, err = client.Metadata(ctx, metadataIDs)
		if err != nil {
			return result, fmt.Errorf("read frozen remote metadata: %w", err)
		}
	}
	for _, record := range state.Files {
		entry, ok := remote[record.Name]
		if !ok || entry.IsDir {
			continue
		}
		info, metadataOK := metadata[entry.FsID]
		actualSize := entry.Size
		if record.MD5 != "" {
			if metadataOK {
				actualSize = info.Size
			} else {
				result.FrozenMetadataMissing = append(result.FrozenMetadataMissing, record.Name)
			}
		}
		if actualSize == record.Size {
			result.FrozenSizeMatched++
		} else {
			result.FrozenSizeMismatches = append(result.FrozenSizeMismatches, SizeMismatch{Name: record.Name, Expected: record.Size, Actual: actualSize})
		}
		if record.MD5 == "" {
			continue
		}
		if !metadataOK || info.MD5 == "" {
			result.FrozenChecksumUnavailable = append(result.FrozenChecksumUnavailable, record.Name)
			continue
		}
		if strings.EqualFold(info.MD5, record.MD5) || (record.RemoteMD5 != "" && strings.EqualFold(info.MD5, record.RemoteMD5)) {
			result.FrozenChecksumMatched++
			continue
		}
		if record.RemoteMD5 == "" {
			result.FrozenChecksumUnavailable = append(result.FrozenChecksumUnavailable, record.Name)
		} else {
			result.FrozenChecksumMismatches = append(result.FrozenChecksumMismatches, record.Name)
		}
	}
	sort.Slice(result.FrozenSizeMismatches, func(i, j int) bool { return result.FrozenSizeMismatches[i].Name < result.FrozenSizeMismatches[j].Name })
	sort.Strings(result.FrozenMetadataMissing)
	sort.Strings(result.FrozenChecksumUnavailable)
	sort.Strings(result.FrozenChecksumMismatches)
	if len(result.FrozenSizeMismatches) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("%d frozen files have remote size mismatches", len(result.FrozenSizeMismatches)))
	}
	if len(result.FrozenMetadataMissing) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("%d frozen files are missing authoritative remote metadata", len(result.FrozenMetadataMissing)))
	}
	if len(result.FrozenChecksumMismatches) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("%d frozen files have remote checksum mismatches", len(result.FrozenChecksumMismatches)))
	}
	if cfg.RequireChecksum && len(result.FrozenChecksumUnavailable) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("%d frozen files lack auditable remote checksums", len(result.FrozenChecksumUnavailable)))
	}

	localMatches, err := filepath.Glob(filepath.Join(state.SourceDir, state.Pattern))
	if err != nil {
		return result, err
	}
	for _, local := range localMatches {
		if isControlFile(local, stateFile, cfg.ReceiptFile) {
			continue
		}
		result.LocalMatching = append(result.LocalMatching, filepath.Base(local))
	}
	sort.Strings(result.LocalMatching)
	if cfg.RequireLocalEmpty && len(result.LocalMatching) > 0 {
		result.Issues = append(result.Issues, fmt.Sprintf("local source still contains %d matching files", len(result.LocalMatching)))
	}

	if cfg.ReceiptFile != "" {
		result.Receipt.Checked = true
		receipt, receiptErr := runreceipt.VerifySucceeded(cfg.ReceiptFile, stateFile, "upload-batch")
		result.Receipt.Status = receipt.Status
		result.Receipt.ExitCode = receipt.ExitCode
		if receiptErr != nil {
			result.Receipt.Error = receiptErr.Error()
			result.Issues = append(result.Issues, "run receipt is not a verified exit-code-0 result")
		}
	}
	finalStateSHA256, err := runreceipt.FileSHA256(stateFile)
	if err != nil {
		return result, fmt.Errorf("hash upload state after audit: %w", err)
	}
	if result.StateSHA256 != finalStateSHA256 {
		result.Issues = append(result.Issues, "upload state changed during audit")
	}

	result.Passed = len(result.Issues) == 0
	if !result.Passed {
		return result, fmt.Errorf("batch audit failed: %s", strings.Join(result.Issues, "; "))
	}
	return result, nil
}

func isControlFile(filePath, stateFile, receiptFile string) bool {
	filePath = filepath.Clean(filePath)
	defaultReceipt := stateFile + ".receipt.json"
	control := []string{
		stateFile, stateFile + ".tmp", stateFile + ".lock",
		defaultReceipt, defaultReceipt + ".tmp", defaultReceipt + ".lock",
	}
	if receiptFile != "" {
		control = append(control, receiptFile, receiptFile+".tmp", receiptFile+".lock")
	}
	for _, candidate := range control {
		if filePath == filepath.Clean(candidate) {
			return true
		}
	}
	return false
}

func GenerateExpected(template string, start, end int) ([]string, error) {
	if template == "" {
		return nil, errors.New("expected template is empty")
	}
	if start < 0 || end < 0 {
		return nil, errors.New("expected sequence indexes must be non-negative")
	}
	if end < start {
		return nil, fmt.Errorf("expected end %d is before start %d", end, start)
	}
	if int64(end)-int64(start)+1 > 1_000_000 {
		return nil, errors.New("expected sequence exceeds one million names")
	}
	result := make([]string, 0, end-start+1)
	seen := make(map[string]struct{}, end-start+1)
	for index := start; index <= end; index++ {
		name := fmt.Sprintf(template, index)
		if strings.Contains(name, "%!") {
			return nil, fmt.Errorf("invalid expected template %q", template)
		}
		if err := validateExpectedName(name); err != nil {
			return nil, err
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("expected template produces duplicate name %q", name)
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result, nil
}

func LoadExpectedList(filePath string) ([]string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var result []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		result = append(result, name)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func normalizeExpected(names []string, state *batchupload.State) (map[string]struct{}, error) {
	if len(names) == 0 {
		names = make([]string, 0, len(state.Files))
		for _, record := range state.Files {
			names = append(names, record.Name)
		}
	}
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		if err := validateExpectedName(name); err != nil {
			return nil, err
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("duplicate expected name %q", name)
		}
		result[name] = struct{}{}
	}
	return result, nil
}

func validateExpectedName(name string) error {
	if name == "" || name == "." || name == ".." || path.Base(name) != name || strings.Contains(name, "\\") {
		return fmt.Errorf("expected name must be a safe basename: %q", name)
	}
	return nil
}
