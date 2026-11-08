package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ThinkingCodec carries hidden reasoning between turns as a Fernet token
// under the key file every Splash server of this user shares
// (server/thinking.py), so Python and Go servers read each other's.
type ThinkingCodec struct {
	signing, encryption []byte
}

const maxSignatureBytes = 16 * 1024 * 1024

func NewThinkingCodec(key []byte) (*ThinkingCodec, error) {
	raw, err := base64.URLEncoding.DecodeString(string(key))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("invalid Fernet key")
	}
	return &ThinkingCodec{signing: raw[:16], encryption: raw[16:]}, nil
}

func (c *ThinkingCodec) Encode(text string) (string, error) {
	payload := []byte(text)
	if 4*((57+16*(len(payload)/16+1)+2)/3) > maxSignatureBytes {
		return "", apiErrCode(500, "thinking exceeds the signature size limit", "thinking_too_large")
	}
	iv := make([]byte, 16)
	_, _ = rand.Read(iv)
	pad := 16 - len(payload)%16
	padded := append(append([]byte(nil), payload...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(c.encryption)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	msg := make([]byte, 0, 1+8+16+len(ct)+32)
	msg = append(msg, 0x80)
	msg = binary.BigEndian.AppendUint64(msg, uint64(time.Now().Unix()))
	msg = append(msg, iv...)
	msg = append(msg, ct...)
	mac := hmac.New(sha256.New, c.signing)
	mac.Write(msg)
	msg = mac.Sum(msg)
	return base64.URLEncoding.EncodeToString(msg), nil
}

var errSignature = apiErrCode(400, "invalid signature in thinking block; hidden thinking must be returned to a server using the key that generated it", "invalid_thinking_signature")

func (c *ThinkingCodec) Decode(signature string) (string, error) {
	if len(signature) > maxSignatureBytes {
		return "", apiErrCode(400, "invalid signature in thinking block", "invalid_thinking_signature")
	}
	data, err := base64.URLEncoding.DecodeString(signature)
	if err != nil || len(data) < 1+8+16+16+32 || data[0] != 0x80 || (len(data)-1-8-16-32)%16 != 0 {
		return "", errSignature
	}
	body, tag := data[:len(data)-32], data[len(data)-32:]
	mac := hmac.New(sha256.New, c.signing)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), tag) {
		return "", errSignature
	}
	iv, ct := body[9:25], body[25:]
	block, _ := aes.NewCipher(c.encryption)
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > 16 || !bytes.Equal(plain[len(plain)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return "", errSignature
	}
	return string(plain[:len(plain)-pad]), nil
}

// LoadThinkingKey reads, or creates, the shared key file.
func LoadThinkingKey() ([]byte, error) {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, "Library/Application Support/Splash/thinking.key")
	read := func() ([]byte, error) {
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		st, _ := info.Sys().(*syscall.Stat_t)
		mode := info.Mode().Perm()
		if !info.Mode().IsRegular() || (st != nil && int(st.Uid) != os.Getuid()) || (mode != 0o400 && mode != 0o600) {
			return nil, fmt.Errorf("Thinking key must be a user-owned regular file with mode 0400 or 0600: %s", path)
		}
		key := make([]byte, 45)
		n, _ := f.Read(key)
		key = key[:n]
		if _, err := NewThinkingCodec(key); err != nil || n != 44 {
			return nil, fmt.Errorf("Invalid thinking key: %s", path)
		}
		return key, nil
	}
	key, err := read()
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("Cannot use thinking key %s: %v", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".thinking-")
	if err != nil {
		return nil, fmt.Errorf("Cannot use thinking key %s: %v", path, err)
	}
	defer os.Remove(tmp.Name())
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	_ = tmp.Chmod(0o600)
	_, _ = tmp.Write([]byte(base64.URLEncoding.EncodeToString(raw)))
	_ = tmp.Sync()
	tmp.Close()
	_ = os.Link(tmp.Name(), path)
	return read()
}
