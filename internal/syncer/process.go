package syncer

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

const (
	SyncthingIdentityKeyCredential = "syncthing-identity-key"
	managedRuntimePrefix           = "syncthing-"
	managedRuntimeMarker           = ".remote-docker-runtime"
	identityBlobMagic              = "RDSI1"
)

var ErrIdentityCorrupt = errors.New("stored Syncthing identity is unusable")

// Identity is the Syncthing TLS device identity held only in encrypted form at rest.
type Identity struct {
	CertificatePEM []byte `json:"certificate_pem"`
	PrivateKeyPEM  []byte `json:"private_key_pem"`
}

// EncryptIdentity seals an identity with an AES-256-GCM key held by the OS credential store.
func EncryptIdentity(identity Identity, key []byte, randomness io.Reader) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("Syncthing identity encryption key must be 32 bytes")
	}
	if len(identity.CertificatePEM) == 0 || len(identity.PrivateKeyPEM) == 0 || randomness == nil {
		return nil, errors.New("Syncthing identity is incomplete")
	}
	plaintext, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("encode Syncthing identity: %w", err)
	}
	defer clear(plaintext)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create identity cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create identity AEAD: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(randomness, nonce); err != nil {
		return nil, fmt.Errorf("generate identity nonce: %w", err)
	}
	result := append([]byte(identityBlobMagic), nonce...)
	return gcm.Seal(result, nonce, plaintext, nil), nil
}

func decryptIdentity(blob, key []byte) (Identity, error) {
	if len(key) != 32 {
		return Identity{}, errors.New("Syncthing identity encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Identity{}, fmt.Errorf("create identity cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Identity{}, fmt.Errorf("create identity AEAD: %w", err)
	}
	headerLength := len(identityBlobMagic) + gcm.NonceSize()
	if len(blob) < headerLength || string(blob[:len(identityBlobMagic)]) != identityBlobMagic {
		return Identity{}, errors.New("invalid encrypted Syncthing identity")
	}
	nonce := blob[len(identityBlobMagic):headerLength]
	plaintext, err := gcm.Open(nil, nonce, blob[headerLength:], nil)
	if err != nil {
		return Identity{}, errors.New("decrypt Syncthing identity")
	}
	defer clear(plaintext)
	var identity Identity
	if err := json.Unmarshal(plaintext, &identity); err != nil {
		return Identity{}, errors.New("decode Syncthing identity")
	}
	if len(identity.CertificatePEM) == 0 || len(identity.PrivateKeyPEM) == 0 {
		return Identity{}, errors.New("decrypted Syncthing identity is incomplete")
	}
	return identity, nil
}

// ValidateEncryptedIdentity verifies that one stored blob and credential key
// form a usable identity without exposing decryption details to callers.
func ValidateEncryptedIdentity(blob, key []byte) error {
	identity, err := decryptIdentity(blob, key)
	if err != nil {
		return ErrIdentityCorrupt
	}
	clear(identity.CertificatePEM)
	clear(identity.PrivateKeyPEM)
	return nil
}

// ChildProcess is the one concrete Syncthing process started by this agent.
type ChildProcess interface {
	Wait() error
	Interrupt() error
	Kill() error
}

// ProcessLauncher starts the bundled Syncthing binary.
type ProcessLauncher interface {
	Start(context.Context, string, []string) (ChildProcess, error)
}

// ProcessOptions separates persistent non-secret data from private runtime identity files.
type ProcessOptions struct {
	Executable          string
	PersistentConfigDir string
	DataDir             string
	RuntimeRoot         string
	GUIAddress          string
	DeviceID            string
	Secrets             credentials.Store
	EncryptedIdentity   []byte
	Launcher            ProcessLauncher
}

// ManagedProcess owns one child and one marked private runtime directory.
type ManagedProcess struct {
	runtimeDir          string
	persistentConfigDir string
	child               ChildProcess
	done                chan struct{}
	waitErr             error
	cleanupOnce         sync.Once
}

// StartManagedProcess materializes the encrypted identity and starts only the bundled child.
func StartManagedProcess(ctx context.Context, options ProcessOptions) (*ManagedProcess, error) {
	if err := validateProcessOptions(options); err != nil {
		return nil, err
	}
	for _, directory := range []string{options.PersistentConfigDir, options.DataDir, options.RuntimeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create Syncthing directory %s: %w", directory, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("protect Syncthing directory %s: %w", directory, err)
		}
	}
	if err := CleanupStaleRuntime(options.RuntimeRoot); err != nil {
		return nil, err
	}
	for _, name := range []string{"cert.pem", "key.pem"} {
		if err := os.Remove(filepath.Join(options.PersistentConfigDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove persistent identity file %s: %w", name, err)
		}
	}

	key, err := options.Secrets.Get(options.DeviceID, SyncthingIdentityKeyCredential)
	if err != nil {
		return nil, fmt.Errorf("read Syncthing identity credential: %w", err)
	}
	defer clear(key)
	identity, err := decryptIdentity(options.EncryptedIdentity, key)
	if err != nil {
		return nil, err
	}
	defer clear(identity.PrivateKeyPEM)

	runtimeDir, err := os.MkdirTemp(options.RuntimeRoot, managedRuntimePrefix)
	if err != nil {
		return nil, fmt.Errorf("create Syncthing private runtime: %w", err)
	}
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = os.RemoveAll(runtimeDir)
		}
	}()
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect Syncthing private runtime: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, managedRuntimeMarker), []byte("1"), 0o600); err != nil {
		return nil, fmt.Errorf("mark Syncthing private runtime: %w", err)
	}
	if err := copyPersistentConfig(options.PersistentConfigDir, runtimeDir); err != nil {
		return nil, err
	}
	runtimeConfigPath := filepath.Join(runtimeDir, "config.xml")
	if _, err := os.Stat(runtimeConfigPath); err == nil {
		apiKey, keyErr := options.Secrets.Get(options.DeviceID, SyncthingAPIKeyCredential)
		if keyErr != nil {
			return nil, fmt.Errorf("read Syncthing API credential: %w", keyErr)
		}
		runtimeConfig, readErr := os.ReadFile(runtimeConfigPath)
		if readErr != nil {
			clear(apiKey)
			return nil, readErr
		}
		materialized, rewriteErr := MaterializeConfigAPIKey(runtimeConfig, apiKey)
		clear(apiKey)
		if rewriteErr != nil {
			return nil, rewriteErr
		}
		if err := os.WriteFile(runtimeConfigPath, materialized, 0o600); err != nil {
			return nil, fmt.Errorf("materialize Syncthing API credential: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect Syncthing runtime config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "cert.pem"), identity.CertificatePEM, 0o600); err != nil {
		return nil, fmt.Errorf("materialize Syncthing certificate: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "key.pem"), identity.PrivateKeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("materialize Syncthing private key: %w", err)
	}

	launcher := options.Launcher
	if launcher == nil {
		launcher = commandLauncher{}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start Syncthing: %w", err)
	}
	args := []string{
		"--no-browser",
		"--no-restart",
		"--no-upgrade",
		"--gui-address=" + options.GUIAddress,
		"--config=" + runtimeDir,
		"--data=" + options.DataDir,
	}
	child, err := launcher.Start(context.WithoutCancel(ctx), options.Executable, args)
	if err != nil {
		return nil, fmt.Errorf("start Syncthing: %w", err)
	}

	process := &ManagedProcess{
		runtimeDir:          runtimeDir,
		persistentConfigDir: options.PersistentConfigDir,
		child:               child,
		done:                make(chan struct{}),
	}
	cleanupOnError = false
	go process.monitor()
	return process, nil
}

// RuntimeDir is exposed for diagnostics metadata, never for copying its contents.
func (p *ManagedProcess) RuntimeDir() string { return p.runtimeDir }

// MarkReady deletes plaintext identity files after Syncthing has loaded them.
func (p *ManagedProcess) MarkReady() error {
	for _, name := range []string{"cert.pem", "key.pem"} {
		if err := os.Remove(filepath.Join(p.runtimeDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove runtime identity %s: %w", name, err)
		}
	}
	return nil
}

// Stop interrupts and waits for this process only, killing it only after context expiry.
func (p *ManagedProcess) Stop(ctx context.Context) error {
	if err := p.child.Interrupt(); err != nil {
		return fmt.Errorf("interrupt Syncthing child: %w", err)
	}
	select {
	case <-p.done:
		return p.waitErr
	case <-ctx.Done():
		if err := p.child.Kill(); err != nil {
			return fmt.Errorf("kill Syncthing child after timeout: %w", err)
		}
		<-p.done
		return ctx.Err()
	}
}

// Wait observes an unexpected child exit and its completed secret cleanup.
func (p *ManagedProcess) Wait(ctx context.Context) error {
	select {
	case <-p.done:
		return p.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ManagedProcess) monitor() {
	p.waitErr = p.child.Wait()
	p.cleanupOnce.Do(func() {
		_ = persistRuntimeConfig(p.runtimeDir, p.persistentConfigDir)
		_ = os.RemoveAll(p.runtimeDir)
	})
	close(p.done)
}

func validateProcessOptions(options ProcessOptions) error {
	if strings.TrimSpace(options.Executable) == "" || strings.TrimSpace(options.DeviceID) == "" ||
		options.Secrets == nil || len(options.EncryptedIdentity) == 0 {
		return errors.New("Syncthing process options are incomplete")
	}
	host, _, err := net.SplitHostPort(options.GUIAddress)
	if err != nil {
		return fmt.Errorf("parse Syncthing GUI address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("Syncthing GUI must bind a literal loopback address")
	}
	configDir, err := filepath.Abs(options.PersistentConfigDir)
	if err != nil {
		return err
	}
	runtimeRoot, err := filepath.Abs(options.RuntimeRoot)
	if err != nil {
		return err
	}
	if containsFilesystemPath(configDir, runtimeRoot) || containsFilesystemPath(runtimeRoot, configDir) {
		return errors.New("Syncthing persistent config and private runtime must be separate")
	}
	return nil
}

func containsFilesystemPath(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && !filepath.IsAbs(relative) &&
		(relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

// CleanupStaleRuntime removes only app-marked children below the exact runtime root.
func CleanupStaleRuntime(runtimeRoot string) error {
	absolute, err := filepath.Abs(runtimeRoot)
	if err != nil {
		return err
	}
	if absolute == filepath.VolumeName(absolute)+string(filepath.Separator) {
		return errors.New("refusing to clean a filesystem root")
	}
	entries, err := os.ReadDir(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Syncthing runtime root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), managedRuntimePrefix) {
			continue
		}
		directory := filepath.Join(absolute, entry.Name())
		if _, err := os.Stat(filepath.Join(directory, managedRuntimeMarker)); err != nil {
			continue
		}
		if err := os.RemoveAll(directory); err != nil {
			return fmt.Errorf("remove stale Syncthing runtime %s: %w", directory, err)
		}
	}
	return nil
}

func copyPersistentConfig(persistentDir, runtimeDir string) error {
	source := filepath.Join(persistentDir, "config.xml")
	contents, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read persistent Syncthing config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "config.xml"), contents, 0o600); err != nil {
		return fmt.Errorf("materialize Syncthing config: %w", err)
	}
	return nil
}

func persistRuntimeConfig(runtimeDir, persistentDir string) error {
	contents, err := os.ReadFile(filepath.Join(runtimeDir, "config.xml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sanitized, err := SanitizeConfigAPIKey(contents)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(persistentDir, "config.xml"), sanitized, 0o600)
}

// MaterializeConfigAPIKey injects the credential into a private runtime copy.
func MaterializeConfigAPIKey(config, apiKey []byte) ([]byte, error) {
	if len(apiKey) == 0 {
		return nil, errors.New("Syncthing API credential is empty")
	}
	return rewriteXMLText(config, map[string]string{"configuration/gui/apikey": string(apiKey)})
}

// SanitizeConfigAPIKey removes the credential before configuration is persisted.
func SanitizeConfigAPIKey(config []byte) ([]byte, error) {
	return rewriteXMLText(config, map[string]string{"configuration/gui/apikey": ""})
}

type commandLauncher struct{}

func (commandLauncher) Start(ctx context.Context, executable string, args []string) (ChildProcess, error) {
	command := exec.CommandContext(ctx, executable, args...)
	if err := command.Start(); err != nil {
		return nil, err
	}
	return commandChild{command: command}, nil
}

type commandChild struct{ command *exec.Cmd }

func (c commandChild) Wait() error      { return c.command.Wait() }
func (c commandChild) Interrupt() error { return c.command.Process.Signal(os.Interrupt) }
func (c commandChild) Kill() error      { return c.command.Process.Kill() }
