package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/levmv/golems/pkg/filesearch"
	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/llm"
)

const (
	defaultReadLines    = 200
	maxReadLines        = 2000
	maxReadOutputBytes  = 256 * 1024
	maxReadableFileSize = 64 * 1024 * 1024
	maxEditableFileSize = 8 * 1024 * 1024
	maxGrepMatches      = 100
	maxGlobMatches      = 500
	maxSearchOutput     = 256 * 1024
)

func (w *workspaceTools) read(_ context.Context, call llm.ToolCall) (golem.ToolResult, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeToolArgs(call, &args); err != nil {
		return golem.ToolResult{}, err
	}
	abs, display, err := w.resolveReadableFile(args.Path)
	if err != nil {
		return golem.ToolResult{}, err
	}
	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := clampLimit(args.Limit, defaultReadLines, maxReadLines)
	content, next, more, digest, err := readNumberedLines(abs, offset, limit)
	if err != nil {
		return golem.ToolResult{}, err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "sha256: %s\n", digest)
	if more {
		fmt.Fprintf(&out, "continue: read(path=%q, offset=%d, limit=%d)\n", display, next, limit)
	}
	out.WriteByte('\n')
	if content == "" {
		out.WriteString("no lines\n")
	} else {
		out.WriteString(content)
	}
	return golem.ToolResult{Content: out.String()}, nil
}

func readNumberedLines(path string, offset, limit int) (content string, next int, more bool, digest string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", offset, false, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", offset, false, "", err
	}
	if info.Size() > maxReadableFileSize {
		return "", offset, false, "", fmt.Errorf("file exceeds %d-byte limit", maxReadableFileSize)
	}

	reader := bufio.NewReader(io.LimitReader(file, maxReadableFileSize+1))
	hash := sha256.New()
	var out strings.Builder
	lineNumber := 0
	returned := 0
	var bytesRead int64
	outputFull := false
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) > 0 {
			bytesRead += int64(len(raw))
			if bytesRead > maxReadableFileSize {
				return "", offset, false, "", fmt.Errorf("file exceeds %d-byte limit", maxReadableFileSize)
			}
			_, _ = hash.Write(raw)
			if bytes.IndexByte(raw, 0) >= 0 {
				return "", offset, false, "", errors.New("file appears to be binary")
			}
			if !utf8.Valid(raw) {
				return "", offset, false, "", errors.New("file is not valid UTF-8")
			}

			lineNumber++
			if lineNumber >= offset && returned < limit && !outputFull {
				line := raw
				if line[len(line)-1] == '\n' {
					line = line[:len(line)-1]
				}
				prefix := fmt.Sprintf("%6d\t", lineNumber)
				if out.Len()+len(prefix)+len(line)+1 <= maxReadOutputBytes {
					out.WriteString(prefix)
					_, _ = out.Write(line)
					out.WriteByte('\n')
					returned++
				} else if returned == 0 {
					out.WriteString(truncateNumberedLine(lineNumber, line))
					returned++
					outputFull = true
				} else {
					outputFull = true
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", offset, false, "", readErr
		}
	}
	digest = hex.EncodeToString(hash.Sum(nil))
	next = offset + returned
	more = next <= lineNumber
	return out.String(), next, more, digest, nil
}

func truncateNumberedLine(lineNumber int, line []byte) string {
	prefix := fmt.Sprintf("%6d\t", lineNumber)
	const suffix = "… [line truncated]\n"
	available := max(0, maxReadOutputBytes-len(prefix)-len(suffix))
	cut := min(len(line), available)
	for cut > 0 && !utf8.Valid(line[:cut]) {
		cut--
	}
	return prefix + string(line[:cut]) + suffix
}

func (w *workspaceTools) grep(ctx context.Context, call llm.ToolCall) (golem.ToolResult, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Include string `json:"include"`
	}
	if err := decodeToolArgs(call, &args); err != nil {
		return golem.ToolResult{}, err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return golem.ToolResult{}, errors.New("pattern is required")
	}
	_, display, err := w.resolvePath(args.Path)
	if err != nil {
		return golem.ToolResult{}, err
	}
	lines, truncated, oversizedLines, err := w.runGrep(ctx, args.Pattern, args.Include, display)
	if err != nil {
		return golem.ToolResult{}, err
	}
	if len(lines) == 0 {
		if oversizedLines == 0 {
			return golem.ToolResult{Content: "no matches\n"}, nil
		}
		return golem.ToolResult{Content: fmt.Sprintf("no matches\nskipped_oversized_lines: %d\n", oversizedLines)}, nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "matches: %d\n", len(lines))
	if truncated {
		out.WriteString("truncated: true\n")
	}
	if oversizedLines > 0 {
		fmt.Fprintf(&out, "skipped_oversized_lines: %d\n", oversizedLines)
	}
	out.WriteByte('\n')
	out.WriteString(strings.Join(lines, "\n"))
	out.WriteByte('\n')
	return golem.ToolResult{Content: out.String()}, nil
}

func (w *workspaceTools) runGrep(ctx context.Context, pattern, include, display string) ([]string, bool, int, error) {
	if w.searcher == nil {
		return nil, false, 0, errors.New("grep searcher is not initialized")
	}
	query := filesearch.SearchQuery{
		Path:         display,
		Pattern:      pattern,
		PreviewBytes: 300,
	}
	if strings.TrimSpace(include) != "" {
		query.Include = include
	}
	var lines []string
	usedBytes := 0
	stats, err := w.searcher.Search(ctx, query, func(match filesearch.Match) error {
		if len(lines) >= maxGrepMatches {
			return filesearch.ErrStop
		}
		text := match.Text
		if match.LineTruncated {
			text += " [... omitted end of long line]"
		}
		line := fmt.Sprintf("%s:%d:%s", formatSearchPath(match.Path, display), match.Line, text)
		if usedBytes+len(line)+1 > maxSearchOutput {
			return filesearch.ErrStop
		}
		lines = append(lines, line)
		usedBytes += len(line) + 1
		return nil
	})
	if err != nil {
		return nil, false, 0, err
	}
	return lines, stats.Stopped, stats.OversizedLinesSkipped, nil
}

func (w *workspaceTools) glob(ctx context.Context, call llm.ToolCall) (golem.ToolResult, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := decodeToolArgs(call, &args); err != nil {
		return golem.ToolResult{}, err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return golem.ToolResult{}, errors.New("pattern is required")
	}
	_, display, err := w.resolvePath(args.Path)
	if err != nil {
		return golem.ToolResult{}, err
	}
	paths, truncated, err := w.runGlob(ctx, args.Pattern, display)
	if err != nil {
		return golem.ToolResult{}, err
	}
	if len(paths) == 0 {
		return golem.ToolResult{Content: "no paths\n"}, nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "paths: %d\n", len(paths))
	if truncated {
		out.WriteString("truncated: true\n")
	}
	out.WriteByte('\n')
	out.WriteString(strings.Join(paths, "\n"))
	out.WriteByte('\n')
	return golem.ToolResult{Content: out.String()}, nil
}

func (w *workspaceTools) runGlob(ctx context.Context, pattern, display string) ([]string, bool, error) {
	if w.searcher == nil {
		return nil, false, errors.New("glob searcher is not initialized")
	}
	var paths []string
	usedBytes := 0
	stats, err := w.searcher.Files(ctx, filesearch.FilesQuery{Path: display, Glob: pattern}, func(file filesearch.File) error {
		if len(paths) >= maxGlobMatches {
			return filesearch.ErrStop
		}
		path := formatSearchPath(file.Path, display)
		if usedBytes+len(path)+1 > maxSearchOutput {
			return filesearch.ErrStop
		}
		paths = append(paths, path)
		usedBytes += len(path) + 1
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return paths, stats.Stopped, nil
}

func formatSearchPath(path, queryPath string) string {
	if queryPath == "." {
		return "./" + path
	}
	return path
}

func (w *workspaceTools) edit(_ context.Context, call llm.ToolCall) (golem.ToolResult, error) {
	var args struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := decodeToolArgs(call, &args); err != nil {
		return golem.ToolResult{}, err
	}
	if args.OldText == "" {
		return golem.ToolResult{}, errors.New("old_text must not be empty")
	}
	if !utf8.ValidString(args.OldText) || !utf8.ValidString(args.NewText) {
		return golem.ToolResult{}, errors.New("old_text and new_text must be valid UTF-8")
	}
	abs, display, err := w.resolveReadableFile(args.Path)
	if err != nil {
		return golem.ToolResult{}, err
	}
	old, err := readBoundedTextFile(abs, maxEditableFileSize)
	if err != nil {
		return golem.ToolResult{}, err
	}
	oldHash := sha256.Sum256(old)
	count := bytes.Count(old, []byte(args.OldText))
	if count != 1 {
		return golem.ToolResult{}, fmt.Errorf("old_text occurs %d times in %s; reread and provide one exact occurrence", count, display)
	}
	updated := bytes.Replace(old, []byte(args.OldText), []byte(args.NewText), 1)
	info, err := os.Stat(abs)
	if err != nil {
		return golem.ToolResult{}, err
	}
	if err := atomicReplace(abs, updated, info.Mode().Perm(), &oldHash, false); err != nil {
		return golem.ToolResult{}, err
	}
	newHash := sha256.Sum256(updated)
	change := buildFileChangeMeta(display, "edited", old, updated)
	return golem.ToolResult{Content: formatEditResult(newHash), Meta: change}, nil
}

func (w *workspaceTools) write(_ context.Context, call llm.ToolCall) (golem.ToolResult, error) {
	var args struct {
		Path           string `json:"path"`
		Content        string `json:"content"`
		ExpectedSHA256 string `json:"expected_sha256"`
	}
	if err := decodeToolArgs(call, &args); err != nil {
		return golem.ToolResult{}, err
	}
	if !utf8.ValidString(args.Content) {
		return golem.ToolResult{}, errors.New("content must be valid UTF-8")
	}
	if len(args.Content) > maxEditableFileSize {
		return golem.ToolResult{}, fmt.Errorf("content exceeds %d-byte write limit", maxEditableFileSize)
	}
	abs, display, exists, mode, old, oldHash, err := w.resolveWriteTarget(args.Path)
	if err != nil {
		return golem.ToolResult{}, err
	}
	if args.ExpectedSHA256 != "" {
		if !exists {
			return golem.ToolResult{}, errors.New("expected_sha256 was supplied but the file does not exist")
		}
		if err := verifyExpectedHash(args.ExpectedSHA256, oldHash); err != nil {
			return golem.ToolResult{}, err
		}
	}
	updated := []byte(args.Content)
	var expected *[32]byte
	if exists {
		expected = &oldHash
	}
	if err := atomicReplace(abs, updated, mode, expected, !exists); err != nil {
		return golem.ToolResult{}, err
	}
	newHash := sha256.Sum256(updated)
	operation := "created"
	if exists {
		operation = "replaced"
	}
	change := buildFileChangeMeta(display, operation, old, updated)
	return golem.ToolResult{Content: formatWriteResult(operation, newHash), Meta: change}, nil
}

func (w *workspaceTools) resolveWriteTarget(path string) (abs, display string, exists bool, mode os.FileMode, old []byte, digest [32]byte, err error) {
	abs, display, err = w.resolvePath(path)
	if err != nil {
		return
	}
	if display == "." {
		err = errors.New("file path is required")
		return
	}
	parent := filepath.Dir(abs)
	realParent, parentErr := ensureWritableParent(w.root, parent)
	if parentErr != nil {
		err = fmt.Errorf("prepare parent for %s: %w", display, parentErr)
		return
	}
	abs = filepath.Join(realParent, filepath.Base(abs))
	info, statErr := os.Lstat(abs)
	if errors.Is(statErr, os.ErrNotExist) {
		mode = 0o644
		return abs, display, false, mode, nil, digest, nil
	}
	if statErr != nil {
		err = statErr
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		var target string
		target, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return
		}
		if !isWithinRoot(w.root, target) {
			err = fmt.Errorf("symlink target escapes workspace root: %s", display)
			return
		}
		abs = target
		info, err = os.Stat(abs)
		if err != nil {
			return
		}
	}
	if !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", display)
		return
	}
	old, err = readBoundedTextFile(abs, maxEditableFileSize)
	if err != nil {
		return
	}
	digest = sha256.Sum256(old)
	return abs, display, true, info.Mode().Perm(), old, digest, nil
}

func ensureWritableParent(root, parent string) (string, error) {
	probe := parent
	var missing []string
	for {
		info, err := os.Lstat(probe)
		if err == nil {
			if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				return "", fmt.Errorf("%s is not a directory", probe)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		next := filepath.Dir(probe)
		if next == probe {
			return "", errors.New("no existing parent directory")
		}
		probe = next
	}
	realParent, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	if !isWithinRoot(root, realParent) {
		return "", errors.New("parent symlink escapes workspace root")
	}
	for i := len(missing) - 1; i >= 0; i-- {
		next := filepath.Join(realParent, missing[i])
		if err := os.Mkdir(next, 0o755); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, statErr := os.Lstat(next)
			if statErr != nil {
				return "", statErr
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", errors.New("parent path changed to a symlink while creating directories")
			}
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", next)
			}
		}
		realParent = next
	}
	return realParent, nil
}

func atomicReplace(path string, content []byte, mode os.FileMode, expected *[32]byte, requireAbsent bool) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".cy-write-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if n, err := temp.Write(content); err != nil || n != len(content) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if requireAbsent {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("file appeared while write was being prepared; reread before replacing it")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if expected != nil {
		current, err := readBoundedTextFile(path, maxEditableFileSize)
		if err != nil {
			return err
		}
		if currentHash := sha256.Sum256(current); currentHash != *expected {
			return errors.New("file changed while the update was being prepared; reread before retrying")
		}
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	if err := syncParentDir(dir); err != nil {
		return fmt.Errorf("sync file directory: %w", err)
	}
	ok = true
	return nil
}

func readBoundedTextFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds %d-byte limit", maxBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("file appears to be binary")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("file is not valid UTF-8")
	}
	return data, nil
}

func verifyExpectedHash(raw string, actual [32]byte) error {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return nil
	}
	if len(raw) != 64 {
		return errors.New("expected_sha256 must contain 64 hexadecimal characters")
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return errors.New("expected_sha256 must contain 64 hexadecimal characters")
	}
	if !bytes.Equal(decoded, actual[:]) {
		return fmt.Errorf("file hash is %s, not expected %s; reread before replacing", hex.EncodeToString(actual[:]), raw)
	}
	return nil
}

func formatEditResult(newHash [32]byte) string {
	return fmt.Sprintf("updated\nsha256: %x", newHash)
}

func formatWriteResult(operation string, newHash [32]byte) string {
	return fmt.Sprintf("%s\nsha256: %x", operation, newHash)
}

func minimalToolEnv(home string) []string {
	// Start from an empty environment intentionally. Bash callers provide a
	// per-workspace tool home, while this small allowlist keeps ordinary CLI
	// tools usable without leaking provider keys or other credential carriers.
	// This reduces accidental exposure; it is not a sandbox boundary.
	//
	// TMPDIR is set rather than inherited: pointing it inside the tool home is
	// what lets the sandbox stop granting the machine-wide temp directory,
	// which every other process on the box can also read and write.
	env := []string{"HOME=" + home, "TMPDIR=" + WorkspaceToolTemp(home)}
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TZ"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}
