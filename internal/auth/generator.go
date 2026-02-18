package auth

import (
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"github.com/zeebo/xxh3"

	"marznode/internal/config"
)

func GenerateUUID(seed string, algo config.AuthAlgorithm) (string, error) {
	if algo == config.AuthPlain {
		id, err := uuid.Parse(seed)
		if err != nil {
			return "", err
		}
		return id.String(), nil
	}

	raw := xxh128Bytes(seed)

	id, err := uuid.FromBytes(raw)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func GeneratePassword(seed string, algo config.AuthAlgorithm) string {
	if algo == config.AuthPlain {
		return seed
	}

	return hex.EncodeToString(xxh128Bytes(seed))
}

func UserIdentifier(uid uint32, username string) string {
	return fmt.Sprintf("%d.%s", uid, username)
}

func xxh128Bytes(seed string) []byte {
	// Match Python xxhash.xxh128 digest byte order exactly.
	h := xxh3.Hash128([]byte(seed))
	return []byte{
		byte(h.Hi >> 56),
		byte(h.Hi >> 48),
		byte(h.Hi >> 40),
		byte(h.Hi >> 32),
		byte(h.Hi >> 24),
		byte(h.Hi >> 16),
		byte(h.Hi >> 8),
		byte(h.Hi),
		byte(h.Lo >> 56),
		byte(h.Lo >> 48),
		byte(h.Lo >> 40),
		byte(h.Lo >> 32),
		byte(h.Lo >> 24),
		byte(h.Lo >> 16),
		byte(h.Lo >> 8),
		byte(h.Lo),
	}
}
