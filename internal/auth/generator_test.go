package auth

import (
	"testing"

	"marznode/internal/config"
)

func TestGenerateUUIDXXH128Compatibility(t *testing.T) {
	got, err := GenerateUUID("test-key-123", config.AuthXXH128)
	if err != nil {
		t.Fatalf("GenerateUUID returned error: %v", err)
	}
	const want = "0144d148-af6e-a842-8df3-fc0e004eeca0"
	if got != want {
		t.Fatalf("unexpected uuid: got %s want %s", got, want)
	}
}

func TestGeneratePasswordXXH128Compatibility(t *testing.T) {
	got := GeneratePassword("test-key-123", config.AuthXXH128)
	const want = "0144d148af6ea8428df3fc0e004eeca0"
	if got != want {
		t.Fatalf("unexpected password: got %s want %s", got, want)
	}
}
