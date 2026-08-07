package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeRuntimeIdentityKeyRejectsTrailingOrOversizedInput(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32))
	key, err := decodeRuntimeIdentityKey(strings.NewReader(`{"key":"` + encoded + `"}`))
	if err != nil || !bytes.Equal(key, bytes.Repeat([]byte{0x31}, 32)) {
		t.Fatalf("decode valid key = %x error=%v", key, err)
	}
	for _, input := range []string{
		`{"key":"` + encoded + `"}{"key":"` + encoded + `"}`,
		`{"key":"` + encoded + `","extra":true}`,
		strings.Repeat("x", 4097),
	} {
		if _, err := decodeRuntimeIdentityKey(strings.NewReader(input)); err == nil {
			t.Fatalf("decodeRuntimeIdentityKey() accepted invalid input of length %d", len(input))
		}
	}
}

func TestPrepareRuntimeIdentityPersistsOnlyEncryptedBundleAndPublicMetadata(t *testing.T) {
	root := t.TempDir()
	persistentRoot := filepath.Join(root, "persistent")
	runtimeRoot := filepath.Join(root, "runtime")
	key := sha256.Sum256([]byte("windows-credential-manager-key"))
	generated := generatedRuntimeIdentity{
		Bundle: runtimeIdentityBundle{
			SSHPrivateKey:        []byte("ssh-private-material"),
			SyncthingCertificate: []byte("syncthing-certificate"),
			SyncthingPrivateKey:  []byte("syncthing-private-material"),
			SyncthingAPIKey:      []byte("syncthing-api-secret"),
		},
		SSHHostPublicKey:  []byte("ssh-ed25519 public-host-key\n"),
		SyncthingDeviceID: "WINDOWS-SYNCTHING-ID",
		PersistentConfig:  []byte(`<configuration><gui><apikey></apikey></gui></configuration>`),
	}
	starter := &inspectingRuntimeStarter{inspect: func() error {
		for path, secret := range map[string]string{
			filepath.Join(runtimeRoot, "ssh_host_ed25519_key"):    "ssh-private-material",
			filepath.Join(runtimeRoot, "syncthing", "key.pem"):    "syncthing-private-material",
			filepath.Join(runtimeRoot, "syncthing", "cert.pem"):   "syncthing-certificate",
			filepath.Join(runtimeRoot, "syncthing", "config.xml"): "syncthing-api-secret",
		} {
			contents, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(contents, []byte(secret)) {
				return &runtimeInspectionError{path: path, err: err}
			}
		}
		return nil
	}}
	options := runtimeIdentityOptions{
		PersistentRoot: persistentRoot,
		RuntimeRoot:    runtimeRoot,
		OwnerUID:       -1,
		OwnerGID:       -1,
		Generator:      staticRuntimeIdentityGenerator{identity: generated},
		Starter:        starter,
	}

	if err := prepareRuntimeIdentity(context.Background(), key[:], options); err != nil {
		t.Fatalf("prepareRuntimeIdentity() error = %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("starter calls = %d, want 1", starter.calls)
	}
	for _, path := range []string{
		filepath.Join(runtimeRoot, "ssh_host_ed25519_key"),
		filepath.Join(runtimeRoot, "syncthing", "key.pem"),
		filepath.Join(runtimeRoot, "syncthing", "cert.pem"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("private runtime file remains at %s: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(persistentRoot, "ssh_host_ed25519_key"),
		filepath.Join(persistentRoot, "syncthing", "key.pem"),
		filepath.Join(persistentRoot, "syncthing", "cert.pem"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("private identity persisted at %s: %v", path, err)
		}
	}
	encrypted, err := os.ReadFile(filepath.Join(persistentRoot, "identity.enc"))
	if err != nil {
		t.Fatalf("read encrypted identity: %v", err)
	}
	for _, secret := range []string{"ssh-private-material", "syncthing-private-material", "syncthing-api-secret"} {
		if bytes.Contains(encrypted, []byte(secret)) {
			t.Fatalf("encrypted identity exposes %q", secret)
		}
	}
	persistentConfig, err := os.ReadFile(filepath.Join(persistentRoot, "syncthing", "config.xml"))
	if err != nil || bytes.Contains(persistentConfig, []byte("syncthing-api-secret")) {
		t.Fatalf("persistent config contains runtime API credential: %q error=%v", persistentConfig, err)
	}
	deviceID, err := os.ReadFile(filepath.Join(persistentRoot, "syncthing", "device-id"))
	if err != nil || strings.TrimSpace(string(deviceID)) != "WINDOWS-SYNCTHING-ID" {
		t.Fatalf("device ID = %q error=%v", deviceID, err)
	}
}

func TestPrepareRuntimeIdentityReusesEncryptedBundleWithoutRegeneration(t *testing.T) {
	root := t.TempDir()
	key := sha256.Sum256([]byte("stable-windows-key"))
	generator := &countingRuntimeIdentityGenerator{identity: generatedRuntimeIdentity{
		Bundle: runtimeIdentityBundle{
			SSHPrivateKey: []byte("ssh-private"), SyncthingCertificate: []byte("cert"),
			SyncthingPrivateKey: []byte("sync-private"), SyncthingAPIKey: []byte("api-key"),
		},
		SSHHostPublicKey: []byte("ssh-ed25519 public\n"), SyncthingDeviceID: "DEVICE-ID",
		PersistentConfig: []byte(`<configuration><gui><apikey></apikey></gui></configuration>`),
	}}
	options := runtimeIdentityOptions{
		PersistentRoot: filepath.Join(root, "persistent"), RuntimeRoot: filepath.Join(root, "runtime"),
		OwnerUID: -1, OwnerGID: -1, Generator: generator, Starter: &inspectingRuntimeStarter{},
	}
	if err := prepareRuntimeIdentity(context.Background(), key[:], options); err != nil {
		t.Fatalf("first prepare error = %v", err)
	}
	if err := prepareRuntimeIdentity(context.Background(), key[:], options); err != nil {
		t.Fatalf("second prepare error = %v", err)
	}
	if generator.calls != 1 {
		t.Fatalf("generator calls = %d, want 1", generator.calls)
	}
}

func TestPrepareRuntimeIdentityRejectsWrongWindowsKeyWithoutReplacingIdentity(t *testing.T) {
	root := t.TempDir()
	key := sha256.Sum256([]byte("original-windows-key"))
	wrongKey := sha256.Sum256([]byte("replacement-windows-key"))
	starter := &inspectingRuntimeStarter{}
	options := runtimeIdentityOptions{
		PersistentRoot: filepath.Join(root, "persistent"), RuntimeRoot: filepath.Join(root, "runtime"),
		OwnerUID: -1, OwnerGID: -1, Starter: starter,
		Generator: staticRuntimeIdentityGenerator{identity: completeGeneratedRuntimeIdentity()},
	}
	if err := prepareRuntimeIdentity(context.Background(), key[:], options); err != nil {
		t.Fatalf("first prepare error = %v", err)
	}
	before, err := os.ReadFile(filepath.Join(options.PersistentRoot, "identity.enc"))
	if err != nil {
		t.Fatalf("read encrypted identity: %v", err)
	}
	if err := prepareRuntimeIdentity(context.Background(), wrongKey[:], options); err == nil {
		t.Fatal("prepare accepted the wrong Windows-owned key")
	}
	after, err := os.ReadFile(filepath.Join(options.PersistentRoot, "identity.enc"))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("wrong key replaced the encrypted identity")
	}
	if starter.calls != 1 {
		t.Fatalf("starter calls = %d, want 1", starter.calls)
	}
}

func TestPrepareRuntimeIdentityKeepsLegacySecretsUntilEncryptedBundleOpens(t *testing.T) {
	root := t.TempDir()
	persistentRoot := filepath.Join(root, "persistent")
	identityRoot := filepath.Join(root, "private")
	legacySSH := filepath.Join(root, "ssh_host_ed25519_key")
	legacySync := filepath.Join(persistentRoot, "syncthing")
	if err := os.MkdirAll(identityRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacySync, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		legacySSH,
		filepath.Join(legacySync, "cert.pem"),
		filepath.Join(legacySync, "key.pem"),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("legacy-private-material"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(identityRoot, "identity.enc"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte("windows-key"))
	options := runtimeIdentityOptions{
		PersistentRoot:   persistentRoot,
		IdentityRoot:     identityRoot,
		RuntimeRoot:      filepath.Join(root, "runtime"),
		LegacySSHPrivate: legacySSH,
		OwnerUID:         -1,
		OwnerGID:         -1,
	}
	if err := prepareRuntimeIdentity(context.Background(), key[:], options); err == nil {
		t.Fatal("prepare accepted a corrupt encrypted identity")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("legacy secret was removed before encrypted identity validation at %s: %v", path, err)
		}
	}
}

func TestPrepareRuntimeIdentityCleansAllRuntimeSecretsWhenReadinessFails(t *testing.T) {
	root := t.TempDir()
	key := sha256.Sum256([]byte("windows-key"))
	options := runtimeIdentityOptions{
		PersistentRoot: filepath.Join(root, "persistent"), RuntimeRoot: filepath.Join(root, "runtime"),
		OwnerUID: -1, OwnerGID: -1,
		Generator: staticRuntimeIdentityGenerator{identity: completeGeneratedRuntimeIdentity()},
		Starter:   &inspectingRuntimeStarter{inspect: func() error { return os.ErrDeadlineExceeded }},
	}
	if err := prepareRuntimeIdentity(context.Background(), key[:], options); err == nil {
		t.Fatal("prepare succeeded after readiness failure")
	}
	for _, path := range []string{
		filepath.Join(options.RuntimeRoot, "ssh_host_ed25519_key"),
		filepath.Join(options.RuntimeRoot, "syncthing", "cert.pem"),
		filepath.Join(options.RuntimeRoot, "syncthing", "key.pem"),
		filepath.Join(options.RuntimeRoot, "syncthing", "config.xml"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("runtime secret remains after readiness failure at %s: %v", path, err)
		}
	}
}

func completeGeneratedRuntimeIdentity() generatedRuntimeIdentity {
	return generatedRuntimeIdentity{
		Bundle: runtimeIdentityBundle{
			SSHPrivateKey: []byte("ssh-private"), SyncthingCertificate: []byte("cert"),
			SyncthingPrivateKey: []byte("sync-private"), SyncthingAPIKey: []byte("api-key"),
		},
		SSHHostPublicKey: []byte("ssh-ed25519 public\n"), SyncthingDeviceID: "DEVICE-ID",
		PersistentConfig: []byte(`<configuration><gui><apikey></apikey></gui></configuration>`),
	}
}

type staticRuntimeIdentityGenerator struct{ identity generatedRuntimeIdentity }

func (g staticRuntimeIdentityGenerator) Generate(context.Context, runtimeIdentityOptions) (generatedRuntimeIdentity, error) {
	return g.identity, nil
}

type countingRuntimeIdentityGenerator struct {
	identity generatedRuntimeIdentity
	calls    int
}

func (g *countingRuntimeIdentityGenerator) Generate(context.Context, runtimeIdentityOptions) (generatedRuntimeIdentity, error) {
	g.calls++
	return g.identity, nil
}

type inspectingRuntimeStarter struct {
	inspect func() error
	calls   int
}

func (s *inspectingRuntimeStarter) Start(context.Context) error {
	s.calls++
	if s.inspect != nil {
		return s.inspect()
	}
	return nil
}

type runtimeInspectionError struct {
	path string
	err  error
}

func (e *runtimeInspectionError) Error() string {
	return "runtime identity was not materialized at " + e.path
}
