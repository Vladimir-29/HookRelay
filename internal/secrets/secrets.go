// Package secrets отвечает за генерацию endpoint-секретов и их
// шифрование AES-256-GCM перед сохранением в PostgreSQL.
// Открытый секрет существует только в памяти процесса и в ответе
// API в момент создания/перевыпуска — в БД и логи он не попадает.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// secretLen — длина endpoint-секрета: 32 байта случайных данных.
const secretLen = 32

// nonceLen — стандартный размер nonce для AES-GCM.
const nonceLen = 12

// Box — AEAD-шифр для секретов endpoint. Безопасен для конкурентного
// использования: cipher.AEAD не хранит изменяемого состояния.
type Box struct {
	aead cipher.AEAD
}

// NewBox создаёт Box из 32-байтного мастер-ключа.
func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Generate возвращает криптографически стойкий случайный секрет
// длиной 32 байта. Каждое значение уникально с пренебрежимой
// вероятностью коллизии.
func Generate() ([]byte, error) {
	b := make([]byte, secretLen)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate secret: %w", err)
	}
	return b, nil
}

// Seal шифрует секрет и возвращает nonce||ciphertext+tag —
// единый BYTEA-блоб для хранения в endpoints.secret_encrypted.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	out := b.aead.Seal(nonce, nonce, plaintext, nil)
	return out, nil
}

// Open расшифровывает blob, созданный Seal. Ошибки — неверный ключ,
// повреждённые данные или чужой формат; детали не раскрываются,
// чтобы не помогать атакующему.
func (b *Box) Open(data []byte) ([]byte, error) {
	if len(data) < nonceLen+1 {
		return nil, errors.New("invalid encrypted secret")
	}
	plaintext, err := b.aead.Open(nil, data[:nonceLen], data[nonceLen:], nil)
	if err != nil {
		return nil, errors.New("cannot decrypt secret")
	}
	return plaintext, nil
}
