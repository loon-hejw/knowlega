package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

const salt = "agent-platform.secret-box"

type derivedKey struct {
	current []byte
	legacy  []byte
}

type Box struct {
	primary   derivedKey
	fallbacks []derivedKey
}

func New(material string, fallbacks []string, purpose string) (*Box, error) {
	if strings.TrimSpace(material) == "" {
		return nil, errors.New("secret box key material is required")
	}
	if purpose == "" {
		purpose = "connector-secrets"
	}
	primary, err := derive([]byte(material), purpose)
	if err != nil {
		return nil, err
	}
	box := &Box{primary: primary}
	for _, fallback := range fallbacks {
		if strings.TrimSpace(fallback) == "" {
			continue
		}
		key, err := derive([]byte(fallback), purpose)
		if err != nil {
			return nil, err
		}
		box.fallbacks = append(box.fallbacks, key)
	}
	return box, nil
}

func (b *Box) Encrypt(plain string) (string, error) {
	if b == nil {
		return "", errors.New("secret box is not configured")
	}
	iv := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}
	return encryptWithIV(plain, b.primary.current, iv)
}

func (b *Box) Decrypt(encoded string) (string, error) {
	if b == nil {
		return "", errors.New("secret box is not configured")
	}
	parts := strings.Split(encoded, ":")
	v2 := len(parts) > 0 && parts[0] == "v2"
	if v2 {
		parts = parts[1:]
	}
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("malformed encrypted secret")
	}
	iv, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", errors.New("malformed encrypted secret")
	}
	tag, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("malformed encrypted secret")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("malformed encrypted secret")
	}
	keys := append([]derivedKey{b.primary}, b.fallbacks...)
	for _, key := range keys {
		plain, err := decryptParts(iv, tag, ciphertext, func() []byte {
			if v2 {
				return key.current
			}
			return key.legacy
		}())
		if err == nil {
			return plain, nil
		}
	}
	return "", errors.New("encrypted secret authentication failed")
}

func derive(material []byte, purpose string) (derivedKey, error) {
	reader := hkdf.New(sha256.New, material, []byte(salt), []byte(purpose+".v2"))
	current := make([]byte, 32)
	if _, err := io.ReadFull(reader, current); err != nil {
		return derivedKey{}, err
	}
	legacyHash := sha256.Sum256(material)
	legacy := make([]byte, len(legacyHash))
	copy(legacy, legacyHash[:])
	return derivedKey{current: current, legacy: legacy}, nil
}

func encryptWithIV(plain string, key, iv []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(iv) != gcm.NonceSize() {
		return "", fmt.Errorf("secret box nonce must be %d bytes", gcm.NonceSize())
	}
	sealed := gcm.Seal(nil, iv, []byte(plain), nil)
	tagSize := gcm.Overhead()
	ciphertext, tag := sealed[:len(sealed)-tagSize], sealed[len(sealed)-tagSize:]
	return "v2:" + base64.StdEncoding.EncodeToString(iv) + ":" + base64.StdEncoding.EncodeToString(tag) + ":" + base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptParts(iv, tag, ciphertext, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(iv) != gcm.NonceSize() || len(tag) != gcm.Overhead() {
		return "", errors.New("malformed encrypted secret")
	}
	sealed := append(append([]byte{}, ciphertext...), tag...)
	plain, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
