package service

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/core"
)

func TestReadBoundedSourceFileRejectsOversizedSparseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.md")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(core.MaxSourceBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := readBoundedSourceFile(path); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("error=%v", err)
	}
}

func TestBoundedSourceReaderRejectsOnlyContentBeyondLimit(t *testing.T) {
	exact, err := io.ReadAll(newBoundedReader(strings.NewReader("abc"), 3))
	if err != nil || string(exact) != "abc" {
		t.Fatalf("exact=%q err=%v", exact, err)
	}
	if _, err := io.ReadAll(newBoundedReader(strings.NewReader("abcd"), 3)); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("error=%v", err)
	}
}

func TestAggregateBoundedReaderSharesLimitAcrossFiles(t *testing.T) {
	var used int64
	first, err := io.ReadAll(newAggregateBoundedReader(strings.NewReader("ab"), &used, 3, "upload"))
	if err != nil || string(first) != "ab" {
		t.Fatalf("first=%q used=%d err=%v", first, used, err)
	}
	if _, err := io.ReadAll(newAggregateBoundedReader(strings.NewReader("cd"), &used, 3, "upload")); err == nil || !strings.Contains(err.Error(), "upload exceeds") {
		t.Fatalf("used=%d err=%v", used, err)
	}
}

type readSizeProbe struct {
	remaining int
	maxBuffer int
}

func (r *readSizeProbe) Read(buffer []byte) (int, error) {
	if len(buffer) > r.maxBuffer {
		r.maxBuffer = len(buffer)
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(buffer)
	if n > r.remaining {
		n = r.remaining
	}
	copy(buffer[:n], bytes.Repeat([]byte{'x'}, n))
	r.remaining -= n
	return n, nil
}

func TestUploadSourcesStreamsSourceContent(t *testing.T) {
	root := t.TempDir()
	probe := &readSizeProbe{remaining: 2 << 20}
	result, err := UploadSources(UploadSourcesOptions{
		ProjectPath: root,
		Files:       []UploadSourceInput{{RelativePath: "large.md", Reader: probe}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Uploaded) != 1 || result.Uploaded[0].Size != 2<<20 {
		t.Fatalf("result=%+v", result)
	}
	if probe.maxBuffer > 64<<10 {
		t.Fatalf("source was read with an unexpectedly large buffer: %d", probe.maxBuffer)
	}
}
