package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/hejw/knowledge-core/internal/core"
)

func readBoundedSourceFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > core.MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds maximum size of %d bytes: %s", core.MaxSourceBytes, path)
	}
	data, err := io.ReadAll(io.LimitReader(file, core.MaxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > core.MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds maximum size of %d bytes: %s", core.MaxSourceBytes, path)
	}
	return data, nil
}

func sourceFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type boundedSourceReader struct {
	reader    io.Reader
	remaining int64
}

type aggregateBoundedReader struct {
	reader    io.Reader
	used      *int64
	limit     int64
	limitName string
}

func newBoundedSourceReader(reader io.Reader) io.Reader {
	return newBoundedReader(reader, core.MaxSourceBytes)
}

func newBoundedReader(reader io.Reader, limit int64) io.Reader {
	return &boundedSourceReader{reader: reader, remaining: limit}
}

func newAggregateBoundedReader(reader io.Reader, used *int64, limit int64, limitName string) io.Reader {
	return &aggregateBoundedReader{reader: reader, used: used, limit: limit, limitName: limitName}
}

func (r *boundedSourceReader) Read(buffer []byte) (int, error) {
	if r.remaining > 0 {
		if int64(len(buffer)) > r.remaining {
			buffer = buffer[:r.remaining]
		}
		n, err := r.reader.Read(buffer)
		r.remaining -= int64(n)
		return n, err
	}
	var probe [1]byte
	n, err := r.reader.Read(probe[:])
	if n > 0 {
		return 0, fmt.Errorf("source exceeds maximum size of %d bytes", core.MaxSourceBytes)
	}
	return 0, err
}

func (r *aggregateBoundedReader) Read(buffer []byte) (int, error) {
	remaining := r.limit - *r.used
	if remaining > 0 {
		if int64(len(buffer)) > remaining {
			buffer = buffer[:remaining]
		}
		n, err := r.reader.Read(buffer)
		*r.used += int64(n)
		return n, err
	}
	var probe [1]byte
	n, err := r.reader.Read(probe[:])
	if n > 0 {
		return 0, fmt.Errorf("%s exceeds maximum size of %d bytes", r.limitName, r.limit)
	}
	return 0, err
}
