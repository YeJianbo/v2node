package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	ConfigEnvelopeVersion = "buncloud-config-v1"
	ConfigEnvKey          = "BUNCLOUD_CONFIG_KEY"
	LegacyConfigEnvKey    = "V2NODE_CONFIG_KEY"
)

type ConfigEnvelope struct {
	Version    string `json:"version"`
	Algorithm  string `json:"algorithm"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func GenerateConfigKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

func EncodeConfigKey(key []byte) string {
	return base64.RawStdEncoding.EncodeToString(key)
}

func ParseConfigKey(raw string) ([]byte, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, errors.New("empty config key")
	}

	decodeAttempts := []func(string) ([]byte, error){
		base64.RawStdEncoding.DecodeString,
		base64.StdEncoding.DecodeString,
		hex.DecodeString,
	}

	for _, attempt := range decodeAttempts {
		decoded, err := attempt(value)
		if err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}

	if len(value) == 32 {
		return []byte(value), nil
	}

	return nil, fmt.Errorf("invalid config key length: got %d bytes", len(value))
}

func ReadConfigKeyFromEnv() ([]byte, error) {
	if raw := strings.TrimSpace(os.Getenv(ConfigEnvKey)); raw != "" {
		return ParseConfigKey(raw)
	}
	if raw := strings.TrimSpace(os.Getenv(LegacyConfigEnvKey)); raw != "" {
		return ParseConfigKey(raw)
	}
	paths := []string{
		strings.TrimSpace(os.Getenv("BUNCLOUD_CONFIG_KEY_FILE")),
		strings.TrimSpace(os.Getenv("V2NODE_CONFIG_KEY_FILE")),
		"/etc/.buncloud-agent/config.key",
		"/etc/v2node/config.key",
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		key, err := ParseConfigKey(string(raw))
		if err == nil {
			return key, nil
		}
	}
	return nil, fmt.Errorf("missing config key in %s/%s", ConfigEnvKey, LegacyConfigEnvKey)
}

func EncryptConfig(plain []byte, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plain, nil)
	envelope := ConfigEnvelope{
		Version:    ConfigEnvelopeVersion,
		Algorithm:  "aes-256-gcm",
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	return json.MarshalIndent(envelope, "", "  ")
}

func DecryptConfig(data []byte, key []byte) ([]byte, error) {
	var envelope ConfigEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	if envelope.Version != ConfigEnvelopeVersion {
		return nil, fmt.Errorf("unsupported config envelope version: %s", envelope.Version)
	}
	if envelope.Algorithm != "" && envelope.Algorithm != "aes-256-gcm" {
		return nil, fmt.Errorf("unsupported config envelope algorithm: %s", envelope.Algorithm)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decode nonce failed: %w", err)
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext failed: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt config failed: %w", err)
	}
	return plain, nil
}

func MaybeDecryptConfig(data []byte) ([]byte, error) {
	var envelope ConfigEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Ciphertext == "" || envelope.Nonce == "" {
		return data, nil
	}
	key, err := ReadConfigKeyFromEnv()
	if err != nil {
		return nil, err
	}
	return DecryptConfig(data, key)
}
