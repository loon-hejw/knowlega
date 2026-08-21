package data

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var localFileBlobKey = regexp.MustCompile(`^files/[0-9a-f]{64}$`)
var localTransferBlobID = regexp.MustCompile(`^[0-9a-f]{32}$`)

var (
	ErrLocalFileTooLarge     = errors.New("file exceeds the size limit")
	ErrLocalBlobTooLarge     = errors.New("blob exceeds the size limit")
	ErrLocalBlobHashMismatch = errors.New("blob content hash mismatch")
)

// OpenLocalFileBlob opens the local durable-byte-store layout used by Node:
// <localDir>/files/<sha256>. The root is operator configured; blob keys are
// restricted before joining so an artifact row cannot escape it.
func OpenLocalFileBlob(localDir, blobKey string) (*os.File, int64, error) {
	if !localFileBlobKey.MatchString(blobKey) {
		return nil, 0, nil
	}
	file, err := os.Open(filepath.Join(localDir, "files", blobKey[len("files/"):]))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, 0, nil
	}
	return file, info.Size(), nil
}

// OpenLocalTransferBlob opens Node's local Blob-transfer layout:
// <transferDir>/<32-hex-id>. It intentionally has no sidecar state so Node
// and Go can consume the same staged upload during rollout.
func OpenLocalTransferBlob(transferDir, blobID string) (*os.File, int64, error) {
	if !localTransferBlobID.MatchString(blobID) {
		return nil, 0, nil
	}
	file, err := os.Open(filepath.Join(transferDir, blobID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, 0, nil
	}
	return file, info.Size(), nil
}

func DeleteLocalTransferBlob(transferDir, blobID string) error {
	if !localTransferBlobID.MatchString(blobID) {
		return nil
	}
	err := os.Remove(filepath.Join(transferDir, blobID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func DeleteLocalFileBlob(localDir, blobKey string) error {
	if !localFileBlobKey.MatchString(blobKey) {
		return nil
	}
	err := os.Remove(filepath.Join(localDir, "files", blobKey[len("files/"):]))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// PutLocalTransferBlob is the local raw Blob-transfer store shared with Node:
// <transferDir>/<32-hex-id>. The upload remains opaque until a later consumer
// (such as /v1/files/upload) turns it into a durable file artifact.
func PutLocalTransferBlob(transferDir string, source io.Reader, expectedSHA256 string, maxBytes int64) (string, int64, error) {
	if err := os.MkdirAll(transferDir, 0o755); err != nil {
		return "", 0, err
	}
	blobID, err := newLocalTransferBlobID()
	if err != nil {
		return "", 0, err
	}
	part := filepath.Join(transferDir, blobID+".part")
	finalPath := filepath.Join(transferDir, blobID)
	out, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = os.Remove(part) }()
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	var size int64
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			size += int64(count)
			if maxBytes > 0 && size > maxBytes {
				_ = out.Close()
				return "", 0, ErrLocalBlobTooLarge
			}
			if _, err := hash.Write(buffer[:count]); err != nil {
				_ = out.Close()
				return "", 0, err
			}
			if _, err := out.Write(buffer[:count]); err != nil {
				_ = out.Close()
				return "", 0, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = out.Close()
			return "", 0, readErr
		}
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	if expectedSHA256 != "" && expectedSHA256 != hex.EncodeToString(hash.Sum(nil)) {
		return "", 0, ErrLocalBlobHashMismatch
	}
	if err := os.Rename(part, finalPath); err != nil {
		return "", 0, err
	}
	return blobID, size, nil
}

func newLocalTransferBlobID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

// PutLocalFileBlob writes an immutable Node-compatible docstore blob. The
// sha256 determines its stable key; writes use a sibling temporary file so a
// failed upload never creates a visible partial blob.
func PutLocalFileBlob(localDir string, source io.Reader, maxBytes int64) (string, int64, error) {
	base := filepath.Join(localDir, "files")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", 0, err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", 0, err
	}
	part := filepath.Join(base, "."+hex.EncodeToString(random)+".part")
	out, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = os.Remove(part) }()
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	var size int64
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			size += int64(count)
			if maxBytes > 0 && size > maxBytes {
				_ = out.Close()
				return "", 0, ErrLocalFileTooLarge
			}
			if _, err := hash.Write(buffer[:count]); err != nil {
				_ = out.Close()
				return "", 0, err
			}
			if _, err := out.Write(buffer[:count]); err != nil {
				_ = out.Close()
				return "", 0, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = out.Close()
			return "", 0, readErr
		}
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	key := "files/" + hex.EncodeToString(hash.Sum(nil))
	finalPath := filepath.Join(base, key[len("files/"):])
	if _, err := os.Stat(finalPath); err == nil {
		return key, size, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", 0, err
	}
	if err := os.Rename(part, finalPath); err != nil {
		return "", 0, err
	}
	return key, size, nil
}
