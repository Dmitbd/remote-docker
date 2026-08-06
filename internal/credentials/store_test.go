package credentials

import (
	"bytes"
	"errors"
	"testing"
)

func TestMemoryStoreContract(t *testing.T) {
	testStoreContract(t, NewMemoryStore(), "memory")
}

func testStoreContract(t *testing.T, store Store, namespace string) {
	t.Helper()

	deviceA := namespace + "-device-a"
	deviceB := namespace + "-device-b"
	name := "pairing-token"
	secretA := []byte("secret-a")
	secretB := []byte("secret-b")
	t.Cleanup(func() {
		_ = store.Delete(deviceA, name)
		_ = store.Delete(deviceB, name)
	})

	if err := store.Put(deviceA, name, secretA); err != nil {
		t.Fatalf("Put(device A) error = %v", err)
	}
	if err := store.Put(deviceB, name, secretB); err != nil {
		t.Fatalf("Put(device B) error = %v", err)
	}

	gotA, err := store.Get(deviceA, name)
	if err != nil {
		t.Fatalf("Get(device A) error = %v", err)
	}
	if !bytes.Equal(gotA, secretA) {
		t.Fatalf("Get(device A) = %q, want %q", gotA, secretA)
	}

	gotB, err := store.Get(deviceB, name)
	if err != nil {
		t.Fatalf("Get(device B) error = %v", err)
	}
	if !bytes.Equal(gotB, secretB) {
		t.Fatalf("Get(device B) = %q, want %q", gotB, secretB)
	}

	secretA[0] = 'X'
	gotAAgain, err := store.Get(deviceA, name)
	if err != nil {
		t.Fatalf("Get(device A) after caller mutation error = %v", err)
	}
	if string(gotAAgain) != "secret-a" {
		t.Fatalf("stored secret changed through caller slice: %q", gotAAgain)
	}

	gotAAgain[0] = 'Y'
	gotAFinal, err := store.Get(deviceA, name)
	if err != nil {
		t.Fatalf("Get(device A) after result mutation error = %v", err)
	}
	if string(gotAFinal) != "secret-a" {
		t.Fatalf("stored secret changed through returned slice: %q", gotAFinal)
	}

	if err := store.Delete(deviceA, name); err != nil {
		t.Fatalf("Delete(device A) error = %v", err)
	}
	if _, err := store.Get(deviceA, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(deleted device A) error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(deviceB, name); err != nil {
		t.Fatalf("Get(device B) after deleting device A error = %v", err)
	}

	if err := store.Delete(deviceA, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(missing device A) error = %v, want ErrNotFound", err)
	}
}

func TestStoreRejectsInvalidKeys(t *testing.T) {
	store := NewMemoryStore()
	tests := []struct {
		name     string
		deviceID string
		secret   string
	}{
		{name: "empty device ID", secret: "token"},
		{name: "empty secret name", deviceID: "device"},
		{name: "slash in device ID", deviceID: "device/a", secret: "token"},
		{name: "slash in secret name", deviceID: "device", secret: "pair/token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := store.Put(tt.deviceID, tt.secret, []byte("value")); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Put() error = %v, want ErrInvalidKey", err)
			}
		})
	}
}
