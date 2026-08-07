package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Dmitbd/remote-docker/internal/syncer"
)

const (
	runtimeIdentityMagic   = "RDRI1"
	maxRuntimeIdentityFile = 4 << 20
)

type runtimeIdentityBundle struct {
	SSHPrivateKey        []byte `json:"ssh_private_key"`
	SyncthingCertificate []byte `json:"syncthing_certificate"`
	SyncthingPrivateKey  []byte `json:"syncthing_private_key"`
	SyncthingAPIKey      []byte `json:"syncthing_api_key"`
}

type generatedRuntimeIdentity struct {
	Bundle            runtimeIdentityBundle
	SSHHostPublicKey  []byte
	SyncthingDeviceID string
	PersistentConfig  []byte
}

type runtimeIdentityGenerator interface {
	Generate(context.Context, runtimeIdentityOptions) (generatedRuntimeIdentity, error)
}

type runtimeServiceStarter interface {
	Start(context.Context) error
}

type runtimeIdentityOptions struct {
	PersistentRoot   string
	IdentityRoot     string
	RuntimeRoot      string
	LegacySSHPrivate string
	LegacySSHPublic  string
	OwnerUID         int
	OwnerGID         int
	Random           io.Reader
	Generator        runtimeIdentityGenerator
	Starter          runtimeServiceStarter
}

func defaultRuntimeIdentityOptions() (runtimeIdentityOptions, error) {
	account, err := user.Lookup("remote-docker")
	if err != nil {
		return runtimeIdentityOptions{}, errors.New("managed service user is unavailable")
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return runtimeIdentityOptions{}, errors.New("managed service user is invalid")
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return runtimeIdentityOptions{}, errors.New("managed service group is invalid")
	}
	return runtimeIdentityOptions{
		PersistentRoot: "/var/lib/remote-docker", IdentityRoot: "/var/lib/remote-docker-private", RuntimeRoot: "/run/remote-docker",
		LegacySSHPrivate: "/etc/remote-docker/ssh_host_ed25519_key",
		LegacySSHPublic:  "/etc/remote-docker/ssh_host_ed25519_key.pub",
		OwnerUID:         uid, OwnerGID: gid,
	}, nil
}

func runRuntimePrepare(ctx context.Context, input io.Reader, errorOutput io.Writer) int {
	if os.Geteuid() != 0 {
		_, _ = io.WriteString(errorOutput, "managed runtime preparation requires root\n")
		return 1
	}
	key, err := decodeRuntimeIdentityKey(input)
	if err != nil {
		_, _ = io.WriteString(errorOutput, "managed runtime credential is invalid\n")
		return 1
	}
	defer clear(key)
	options, err := defaultRuntimeIdentityOptions()
	if err != nil {
		_, _ = io.WriteString(errorOutput, "managed runtime is unavailable\n")
		return 1
	}
	if err := prepareRuntimeIdentity(ctx, key, options); err != nil {
		_, _ = io.WriteString(errorOutput, "managed runtime preparation failed\n")
		return 1
	}
	return 0
}

func decodeRuntimeIdentityKey(input io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil || len(data) > 4096 {
		return nil, errors.New("managed runtime credential is invalid")
	}
	var request struct {
		Key string `json:"key"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, errors.New("managed runtime credential is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("managed runtime credential is invalid")
	}
	key, err := base64.StdEncoding.DecodeString(request.Key)
	if err != nil || len(key) != 32 {
		return nil, errors.New("managed runtime credential is invalid")
	}
	return key, nil
}

func prepareRuntimeIdentity(ctx context.Context, key []byte, options runtimeIdentityOptions) error {
	identityRoot := options.IdentityRoot
	if identityRoot == "" {
		identityRoot = options.PersistentRoot
	}
	if len(key) != 32 || !filepath.IsAbs(options.PersistentRoot) || !filepath.IsAbs(options.RuntimeRoot) ||
		!filepath.IsAbs(identityRoot) || filepath.Clean(options.PersistentRoot) != options.PersistentRoot ||
		filepath.Clean(options.RuntimeRoot) != options.RuntimeRoot || filepath.Clean(identityRoot) != identityRoot {
		return errors.New("managed runtime identity options are invalid")
	}
	persistentSync := filepath.Join(options.PersistentRoot, "syncthing")
	runtimeSync := filepath.Join(options.RuntimeRoot, "syncthing")
	directories := []struct {
		path string
		mode os.FileMode
	}{
		{options.PersistentRoot, 0o700}, {persistentSync, 0o700}, {identityRoot, 0o700},
		{options.RuntimeRoot, 0o711}, {runtimeSync, 0o700},
	}
	for _, directory := range directories {
		if err := ensureRuntimeDirectory(directory.path); err != nil {
			return errors.New("create managed runtime identity directory")
		}
		if err := os.Chmod(directory.path, directory.mode); err != nil {
			return errors.New("protect managed runtime identity directory")
		}
	}
	if err := chownManaged(runtimeSync, options); err != nil {
		return err
	}
	if err := chownManaged(persistentSync, options); err != nil {
		return err
	}

	encryptedPath := filepath.Join(identityRoot, "identity.enc")
	if _, err := os.Lstat(encryptedPath); errors.Is(err, os.ErrNotExist) {
		pendingPath := encryptedPath + ".pending"
		if err := cleanupPendingRuntimeIdentity(pendingPath, options); err != nil {
			return err
		}
		generator := options.Generator
		if generator == nil {
			generator = commandRuntimeIdentityGenerator{}
		}
		generated, err := generator.Generate(ctx, options)
		if err != nil || !validGeneratedRuntimeIdentity(generated) {
			return errors.New("generate managed runtime identity")
		}
		defer clearRuntimeIdentity(&generated.Bundle)
		randomness := options.Random
		if randomness == nil {
			randomness = rand.Reader
		}
		encrypted, err := sealRuntimeIdentity(generated.Bundle, key, randomness)
		if err != nil {
			return err
		}
		if err := writeRuntimeFile(pendingPath, encrypted, 0o600, -1, -1); err != nil {
			return err
		}
		if err := writeRuntimeFile(filepath.Join(persistentSync, "config.xml"), generated.PersistentConfig, 0o600, options.OwnerUID, options.OwnerGID); err != nil {
			return err
		}
		if err := writeRuntimeFile(filepath.Join(persistentSync, "device-id"), []byte(strings.TrimSpace(generated.SyncthingDeviceID)+"\n"), 0o600, options.OwnerUID, options.OwnerGID); err != nil {
			return err
		}
		publicPath := options.LegacySSHPublic
		if publicPath == "" {
			publicPath = filepath.Join(options.PersistentRoot, "ssh_host_ed25519_key.pub")
		}
		publicUID, publicGID := -1, -1
		if options.OwnerUID >= 0 && options.OwnerGID >= 0 {
			publicUID, publicGID = 0, 0
		}
		if err := writeRuntimeFile(publicPath, generated.SSHHostPublicKey, 0o644, publicUID, publicGID); err != nil {
			return err
		}
		if err := os.Rename(pendingPath, encryptedPath); err != nil {
			return errors.New("commit encrypted managed runtime identity")
		}
	} else if err != nil {
		return errors.New("inspect encrypted managed runtime identity")
	}
	encrypted, err := readRuntimeFile(encryptedPath)
	if err != nil {
		return err
	}
	bundle, err := openRuntimeIdentity(encrypted, key)
	if err != nil {
		return err
	}
	defer clearRuntimeIdentity(&bundle)
	if err := removeLegacyRuntimeSecrets(options); err != nil {
		return err
	}
	persistentConfig, err := readRuntimeFile(filepath.Join(persistentSync, "config.xml"))
	if err != nil {
		return err
	}
	runtimeConfig, err := syncer.MaterializeConfigAPIKey(persistentConfig, bundle.SyncthingAPIKey)
	if err != nil {
		return err
	}
	materialized := []string{
		filepath.Join(options.RuntimeRoot, "ssh_host_ed25519_key"),
		filepath.Join(runtimeSync, "cert.pem"),
		filepath.Join(runtimeSync, "key.pem"),
		filepath.Join(runtimeSync, "config.xml"),
	}
	ready := false
	rootUID, rootGID := -1, -1
	if options.OwnerUID >= 0 && options.OwnerGID >= 0 {
		rootUID, rootGID = 0, 0
	}
	defer func() {
		for _, path := range materialized[:3] {
			_ = os.Remove(path)
		}
		if !ready {
			_ = os.Remove(materialized[3])
		}
	}()
	if err := writeRuntimeFile(materialized[0], bundle.SSHPrivateKey, 0o600, rootUID, rootGID); err != nil {
		return err
	}
	if err := writeRuntimeFile(materialized[1], bundle.SyncthingCertificate, 0o600, options.OwnerUID, options.OwnerGID); err != nil {
		return err
	}
	if err := writeRuntimeFile(materialized[2], bundle.SyncthingPrivateKey, 0o600, options.OwnerUID, options.OwnerGID); err != nil {
		return err
	}
	if err := writeRuntimeFile(materialized[3], runtimeConfig, 0o600, options.OwnerUID, options.OwnerGID); err != nil {
		return err
	}
	starter := options.Starter
	if starter == nil {
		starter = systemdRuntimeStarter{}
	}
	if err := starter.Start(ctx); err != nil {
		return err
	}
	ready = true
	return nil
}

func validGeneratedRuntimeIdentity(identity generatedRuntimeIdentity) bool {
	return len(identity.Bundle.SSHPrivateKey) != 0 && len(identity.Bundle.SyncthingCertificate) != 0 &&
		len(identity.Bundle.SyncthingPrivateKey) != 0 && len(identity.Bundle.SyncthingAPIKey) != 0 &&
		len(identity.SSHHostPublicKey) != 0 && strings.TrimSpace(identity.SyncthingDeviceID) != "" &&
		len(identity.PersistentConfig) != 0
}

func sealRuntimeIdentity(bundle runtimeIdentityBundle, key []byte, randomness io.Reader) ([]byte, error) {
	plaintext, err := json.Marshal(bundle)
	if err != nil {
		return nil, errors.New("encode managed runtime identity")
	}
	defer clear(plaintext)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("create managed runtime identity cipher")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("create managed runtime identity AEAD")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(randomness, nonce); err != nil {
		return nil, errors.New("generate managed runtime identity nonce")
	}
	sealed := append([]byte(runtimeIdentityMagic), nonce...)
	return gcm.Seal(sealed, nonce, plaintext, nil), nil
}

func openRuntimeIdentity(sealed, key []byte) (runtimeIdentityBundle, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return runtimeIdentityBundle{}, errors.New("create managed runtime identity cipher")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return runtimeIdentityBundle{}, errors.New("create managed runtime identity AEAD")
	}
	header := len(runtimeIdentityMagic) + gcm.NonceSize()
	if len(sealed) < header || string(sealed[:len(runtimeIdentityMagic)]) != runtimeIdentityMagic {
		return runtimeIdentityBundle{}, errors.New("encrypted managed runtime identity is invalid")
	}
	plaintext, err := gcm.Open(nil, sealed[len(runtimeIdentityMagic):header], sealed[header:], nil)
	if err != nil {
		return runtimeIdentityBundle{}, errors.New("decrypt managed runtime identity")
	}
	defer clear(plaintext)
	var bundle runtimeIdentityBundle
	if err := json.Unmarshal(plaintext, &bundle); err != nil || !validGeneratedRuntimeIdentity(generatedRuntimeIdentity{Bundle: bundle, SSHHostPublicKey: []byte{1}, SyncthingDeviceID: "x", PersistentConfig: []byte{1}}) {
		return runtimeIdentityBundle{}, errors.New("decode managed runtime identity")
	}
	return bundle, nil
}

func clearRuntimeIdentity(bundle *runtimeIdentityBundle) {
	clear(bundle.SSHPrivateKey)
	clear(bundle.SyncthingCertificate)
	clear(bundle.SyncthingPrivateKey)
	clear(bundle.SyncthingAPIKey)
}

func readRuntimeFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxRuntimeIdentityFile {
		return nil, errors.New("managed runtime identity file is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("managed runtime identity file is unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("managed runtime identity file changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxRuntimeIdentityFile+1))
	if err != nil || len(contents) > maxRuntimeIdentityFile {
		return nil, errors.New("managed runtime identity file is invalid")
	}
	return contents, nil
}

func writeRuntimeFile(path string, contents []byte, mode os.FileMode, uid, gid int) error {
	if !filepath.IsAbs(path) {
		return errors.New("managed runtime identity path is invalid")
	}
	if err := ensureRuntimeDirectory(filepath.Dir(path)); err != nil {
		return errors.New("create managed runtime identity parent")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".runtime-identity-*")
	if err != nil {
		return errors.New("create managed runtime identity file")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if uid >= 0 && gid >= 0 {
		if err := temporary.Chown(uid, gid); err != nil {
			temporary.Close()
			return err
		}
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func ensureRuntimeDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return errors.New("managed runtime identity directory is invalid")
	}
	return nil
}

func chownManaged(path string, options runtimeIdentityOptions) error {
	if options.OwnerUID < 0 || options.OwnerGID < 0 {
		return nil
	}
	if err := os.Chown(path, options.OwnerUID, options.OwnerGID); err != nil {
		return errors.New("protect managed runtime service directory")
	}
	return nil
}

func removeLegacyRuntimeSecrets(options runtimeIdentityOptions) error {
	for _, path := range []string{
		options.LegacySSHPrivate,
		filepath.Join(options.PersistentRoot, "syncthing", "cert.pem"),
		filepath.Join(options.PersistentRoot, "syncthing", "key.pem"),
	} {
		if path != "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return errors.New("remove legacy managed runtime identity")
			}
		}
	}
	return nil
}

func cleanupPendingRuntimeIdentity(pendingPath string, options runtimeIdentityOptions) error {
	info, err := os.Lstat(pendingPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("pending managed runtime identity is invalid")
	}
	if _, privateErr := os.Stat(options.LegacySSHPrivate); errors.Is(privateErr, os.ErrNotExist) {
		for _, path := range []string{
			filepath.Join(options.PersistentRoot, "syncthing", "config.xml"),
			filepath.Join(options.PersistentRoot, "syncthing", "device-id"),
			options.LegacySSHPublic,
		} {
			if path != "" {
				if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return errors.New("clean pending managed runtime identity")
				}
			}
		}
	} else if privateErr != nil {
		return errors.New("inspect legacy managed runtime identity")
	}
	if err := os.Remove(pendingPath); err != nil {
		return errors.New("clean pending managed runtime identity")
	}
	return nil
}

type commandRuntimeIdentityGenerator struct{}

func (commandRuntimeIdentityGenerator) Generate(ctx context.Context, options runtimeIdentityOptions) (generatedRuntimeIdentity, error) {
	bootstrapRoot, err := os.MkdirTemp(options.RuntimeRoot, "identity-bootstrap-")
	if err != nil {
		return generatedRuntimeIdentity{}, err
	}
	defer os.RemoveAll(bootstrapRoot)
	sshPrivate := filepath.Join(bootstrapRoot, "ssh_host_ed25519_key")
	sshPublic := sshPrivate + ".pub"
	syncHome := filepath.Join(bootstrapRoot, "syncthing")
	legacySync := filepath.Join(options.PersistentRoot, "syncthing")
	legacyPaths := []string{options.LegacySSHPrivate, options.LegacySSHPublic, filepath.Join(legacySync, "cert.pem"), filepath.Join(legacySync, "key.pem"), filepath.Join(legacySync, "config.xml")}
	legacyComplete := true
	legacyPresent := false
	for _, path := range legacyPaths {
		_, statErr := os.Stat(path)
		legacyComplete = legacyComplete && statErr == nil
		legacyPresent = legacyPresent || statErr == nil
	}
	if legacyPresent && !legacyComplete {
		return generatedRuntimeIdentity{}, errors.New("legacy managed runtime identity is incomplete")
	}
	if legacyComplete {
		sshPrivate, sshPublic, syncHome = options.LegacySSHPrivate, options.LegacySSHPublic, legacySync
	} else {
		if err := os.MkdirAll(syncHome, 0o700); err != nil {
			return generatedRuntimeIdentity{}, err
		}
		if err := runIdentityCommand(ctx, "/usr/bin/ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", sshPrivate); err != nil {
			return generatedRuntimeIdentity{}, errors.New("generate managed SSH identity")
		}
		if err := runIdentityCommand(ctx, "/usr/local/bin/syncthing", "generate", "--home="+syncHome, "--no-port-probing"); err != nil {
			return generatedRuntimeIdentity{}, errors.New("generate managed Syncthing identity")
		}
	}
	deviceIDOutput, err := exec.CommandContext(ctx, "/usr/local/bin/syncthing", "device-id", "--home="+syncHome).Output()
	if err != nil || len(deviceIDOutput) > 4096 || strings.TrimSpace(string(deviceIDOutput)) == "" {
		return generatedRuntimeIdentity{}, errors.New("read managed Syncthing device ID")
	}
	config, err := readRuntimeFile(filepath.Join(syncHome, "config.xml"))
	if err != nil {
		return generatedRuntimeIdentity{}, err
	}
	hardened, err := syncer.HardenWSLConfig(config)
	if err != nil {
		return generatedRuntimeIdentity{}, err
	}
	apiKey, err := runtimeConfigAPIKey(hardened)
	if err != nil {
		return generatedRuntimeIdentity{}, err
	}
	persistentConfig, err := syncer.SanitizeConfigAPIKey(hardened)
	if err != nil {
		return generatedRuntimeIdentity{}, err
	}
	sshPrivateData, err := readRuntimeFile(sshPrivate)
	if err != nil {
		return generatedRuntimeIdentity{}, err
	}
	sshPublicData, err := readRuntimeFile(sshPublic)
	if err != nil {
		return generatedRuntimeIdentity{}, err
	}
	certificate, err := readRuntimeFile(filepath.Join(syncHome, "cert.pem"))
	if err != nil {
		return generatedRuntimeIdentity{}, err
	}
	privateKey, err := readRuntimeFile(filepath.Join(syncHome, "key.pem"))
	if err != nil {
		return generatedRuntimeIdentity{}, err
	}
	return generatedRuntimeIdentity{
		Bundle: runtimeIdentityBundle{
			SSHPrivateKey: sshPrivateData, SyncthingCertificate: certificate,
			SyncthingPrivateKey: privateKey, SyncthingAPIKey: apiKey,
		},
		SSHHostPublicKey: sshPublicData, SyncthingDeviceID: strings.TrimSpace(string(deviceIDOutput)),
		PersistentConfig: persistentConfig,
	}, nil
}

func runtimeConfigAPIKey(config []byte) ([]byte, error) {
	var parsed struct {
		GUI struct {
			APIKey string `xml:"apikey"`
		} `xml:"gui"`
	}
	if err := xml.Unmarshal(config, &parsed); err != nil || strings.TrimSpace(parsed.GUI.APIKey) == "" {
		return nil, errors.New("managed Syncthing API credential is unavailable")
	}
	return []byte(strings.TrimSpace(parsed.GUI.APIKey)), nil
}

func runIdentityCommand(ctx context.Context, binary string, args ...string) error {
	command := exec.CommandContext(ctx, binary, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

type systemdRuntimeStarter struct{}

func (systemdRuntimeStarter) Start(ctx context.Context) error {
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", "restart",
		"docker.service", "ssh.service", "syncthing@remote-docker.service", "remote-docker-remote.service", "remote-docker.target")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("start managed runtime services")
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if runtimeServicesReady(readyCtx) {
			return nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			return errors.New("managed runtime services did not become ready")
		case <-timer.C:
		}
	}
}

func runtimeServicesReady(ctx context.Context) bool {
	probes := remoteServiceProbes{}
	if !probes.DockerSocketHealthy(ctx) || !probes.SyncthingServiceHealthy(ctx) {
		return false
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:22")
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func runRuntimeStatus(ctx context.Context) int {
	if runtimeServicesReady(ctx) {
		return 0
	}
	return 1
}

func runRuntimeIdentityState(output io.Writer) int {
	path := "/var/lib/remote-docker-private/identity.enc"
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return 1
	}
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 1
	}
	if err := json.NewEncoder(output).Encode(map[string]bool{"exists": exists}); err != nil {
		return 1
	}
	return 0
}
