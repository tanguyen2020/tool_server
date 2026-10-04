// Package secret encrypts passwords/private keys before they are written to disk.
// Windows uses DPAPI (bound to the current Windows account); other OSes use AES-GCM with a local key file.
package secret

import (
	"encoding/base64"
	"strings"
)

const prefix = "v1:"

// KeyDir is the directory holding the AES key file (non-Windows only; set by store at startup).
var KeyDir string

// Encrypt returns the encrypted value as "v1:<base64>".
func Encrypt(plain string) (string, error) {
	out, err := protect([]byte(plain))
	if err != nil {
		return "", err
	}
	return prefix + base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt decrypts a value produced by Encrypt.
func Decrypt(value string) (string, error) {
	if !strings.HasPrefix(value, prefix) {
		return value, nil
	}
	raw, err := base64.StdEncoding.DecodeString(value[len(prefix):])
	if err != nil {
		return "", err
	}
	out, err := unprotect(raw)
	return string(out), err
}
