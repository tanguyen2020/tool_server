//go:build !windows

package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var (
	keyOnce sync.Once
	key     []byte
	keyErr  error
)

func loadKey() ([]byte, error) {
	keyOnce.Do(func() {
		file := filepath.Join(KeyDir, "secret.key")
		if b, err := os.ReadFile(file); err == nil && len(b) == 32 {
			key = b
			return
		}
		key = make([]byte, 32)
		if _, keyErr = rand.Read(key); keyErr == nil {
			keyErr = os.WriteFile(file, key, 0o600)
		}
	})
	return key, keyErr
}

func gcm() (cipher.AEAD, error) {
	k, err := loadKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func protect(data []byte) ([]byte, error) {
	g, err := gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, data, nil), nil
}

func unprotect(data []byte) ([]byte, error) {
	g, err := gcm()
	if err != nil {
		return nil, err
	}
	if len(data) < g.NonceSize() {
		return nil, errors.New("invalid encrypted data")
	}
	return g.Open(nil, data[:g.NonceSize()], data[g.NonceSize():], nil)
}
