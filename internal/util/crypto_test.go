package util

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestPasswordHashVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("expected match, ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong password", hash)
	if err != nil || ok {
		t.Fatalf("expected mismatch, ok=%v err=%v", ok, err)
	}
}

func TestPasswordHashUniqueSalts(t *testing.T) {
	h1, _ := HashPassword("same")
	h2, _ := HashPassword("same")
	if h1 == h2 {
		t.Fatal("expected different hashes for same password")
	}
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	plaintext := []byte("sasl password hunter2")
	ct, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}
	pt, err := Decrypt(key, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("roundtrip mismatch: %q", pt)
	}
}

func TestDecryptTampered(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	ct, err := Encrypt(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	ct[len(ct)-1] ^= 0xFF
	if _, err := Decrypt(key, ct); err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestLoadOrCreateMasterKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "master.key")
	k1, err := LoadOrCreateMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(k1))
	}
	k2, err := LoadOrCreateMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("key not stable across loads")
	}
}
