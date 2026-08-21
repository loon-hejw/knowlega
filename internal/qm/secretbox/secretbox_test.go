package secretbox

import "testing"

func TestNodeCompatibleV2Ciphertext(t *testing.T) {
	box, err := New("0123456789abcdef0123456789abcdef", nil, "connector-secrets")
	if err != nil {
		t.Fatal(err)
	}
	const encoded = "v2:AAECAwQFBgcICQoL:y+7/RZMFA4iuGCSzu5ljOw==:DhYfNxjNax0C5yRO9PaHYgVUR2c="
	plain, err := box.Decrypt(encoded)
	if err != nil || plain != "viewer-token-example" {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
}

func TestRoundTripAndFallback(t *testing.T) {
	oldBox, err := New("old-secret-material-which-is-long-enough", nil, "connector-secrets")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := oldBox.Encrypt("token-value")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := New("new-secret-material-which-is-long-enough", []string{"old-secret-material-which-is-long-enough"}, "connector-secrets")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := rotated.Decrypt(encoded)
	if err != nil || plain != "token-value" {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
	if _, err := rotated.Decrypt(encoded + "tampered"); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}
