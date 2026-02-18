package auth

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/cespare/xxhash/v2"
	"github.com/google/uuid"

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

	sum1 := xxhash.Sum64String(seed)
	sum2 := xxhash.Sum64String("marznode:" + seed)
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[0:8], sum1)
	binary.BigEndian.PutUint64(raw[8:16], sum2)

	id, err := uuid.FromBytes(raw[:])
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func GeneratePassword(seed string, algo config.AuthAlgorithm) string {
	if algo == config.AuthPlain {
		return seed
	}

	sum1 := xxhash.Sum64String(seed)
	sum2 := xxhash.Sum64String("marznode:" + seed)
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[0:8], sum1)
	binary.BigEndian.PutUint64(raw[8:16], sum2)
	return hex.EncodeToString(raw[:])
}

func UserIdentifier(uid uint32, username string) string {
	return fmt.Sprintf("%d.%s", uid, username)
}
