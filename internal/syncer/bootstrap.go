package syncer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

const maxSyncthingBootstrapFile = 2 << 20

// IdentityGenerator creates one fresh Syncthing identity in a private
// temporary directory and returns its public device ID.
type IdentityGenerator interface {
	Generate(context.Context, string, string) (string, error)
}

type BootstrapOptions struct {
	Executable          string
	PersistentConfigDir string
	RuntimeRoot         string
	CredentialOwner     string
	Secrets             credentials.Store
	Random              io.Reader
	Generator           IdentityGenerator
}

type BootstrapResult struct {
	DeviceID          string
	EncryptedIdentity []byte
}

// BootstrapIdentity generates a fresh identity only in a private temporary
// directory, stores its encryption key and REST API key in the OS credential
// store, and persists only encrypted identity plus a hardened secret-free
// configuration.
func BootstrapIdentity(ctx context.Context, options BootstrapOptions) (BootstrapResult, error) {
	if strings.TrimSpace(options.Executable) == "" || strings.TrimSpace(options.CredentialOwner) == "" || options.Secrets == nil {
		return BootstrapResult{}, errors.New("Syncthing bootstrap options are incomplete")
	}
	configDir, err := filepath.Abs(options.PersistentConfigDir)
	if err != nil {
		return BootstrapResult{}, err
	}
	runtimeRoot, err := filepath.Abs(options.RuntimeRoot)
	if err != nil {
		return BootstrapResult{}, err
	}
	if containsFilesystemPath(configDir, runtimeRoot) || containsFilesystemPath(runtimeRoot, configDir) {
		return BootstrapResult{}, errors.New("Syncthing bootstrap config and runtime paths must be separate")
	}
	for _, directory := range []string{configDir, runtimeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return BootstrapResult{}, fmt.Errorf("create Syncthing bootstrap directory: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return BootstrapResult{}, fmt.Errorf("protect Syncthing bootstrap directory: %w", err)
		}
	}
	temporary, err := os.MkdirTemp(runtimeRoot, "bootstrap-")
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("create Syncthing bootstrap runtime: %w", err)
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return BootstrapResult{}, fmt.Errorf("protect Syncthing bootstrap runtime: %w", err)
	}

	generator := options.Generator
	if generator == nil {
		generator = commandIdentityGenerator{}
	}
	deviceID, err := generator.Generate(ctx, options.Executable, temporary)
	if err != nil || strings.TrimSpace(deviceID) == "" || len(deviceID) > 128 {
		return BootstrapResult{}, errors.New("generate managed Syncthing identity")
	}
	certificate, err := readBootstrapFile(filepath.Join(temporary, "cert.pem"))
	if err != nil {
		return BootstrapResult{}, err
	}
	privateKey, err := readBootstrapFile(filepath.Join(temporary, "key.pem"))
	if err != nil {
		return BootstrapResult{}, err
	}
	defer clear(privateKey)
	configData, err := readBootstrapFile(filepath.Join(temporary, "config.xml"))
	if err != nil {
		return BootstrapResult{}, err
	}
	apiKey, hardenedConfig, err := hardenGeneratedConfig(configData)
	if err != nil {
		return BootstrapResult{}, err
	}
	defer clear(apiKey)

	randomness := options.Random
	if randomness == nil {
		randomness = rand.Reader
	}
	identityKey := make([]byte, 32)
	if _, err := io.ReadFull(randomness, identityKey); err != nil {
		return BootstrapResult{}, fmt.Errorf("generate Syncthing identity key: %w", err)
	}
	defer clear(identityKey)
	encrypted, err := EncryptIdentity(Identity{CertificatePEM: certificate, PrivateKeyPEM: privateKey}, identityKey, randomness)
	if err != nil {
		return BootstrapResult{}, err
	}
	if err := options.Secrets.Put(options.CredentialOwner, SyncthingIdentityKeyCredential, identityKey); err != nil {
		return BootstrapResult{}, fmt.Errorf("store Syncthing identity key: %w", err)
	}
	storedIdentityKey := true
	defer func() {
		if storedIdentityKey {
			_ = options.Secrets.Delete(options.CredentialOwner, SyncthingIdentityKeyCredential)
		}
	}()
	if err := options.Secrets.Put(options.CredentialOwner, SyncthingAPIKeyCredential, apiKey); err != nil {
		return BootstrapResult{}, fmt.Errorf("store Syncthing API key: %w", err)
	}
	storedAPIKey := true
	defer func() {
		if storedAPIKey {
			_ = options.Secrets.Delete(options.CredentialOwner, SyncthingAPIKeyCredential)
		}
	}()
	if err := os.WriteFile(filepath.Join(configDir, "config.xml"), hardenedConfig, 0o600); err != nil {
		return BootstrapResult{}, fmt.Errorf("persist hardened Syncthing config: %w", err)
	}
	storedIdentityKey = false
	storedAPIKey = false
	return BootstrapResult{DeviceID: strings.TrimSpace(deviceID), EncryptedIdentity: encrypted}, nil
}

type commandIdentityGenerator struct{}

func (commandIdentityGenerator) Generate(ctx context.Context, executable, home string) (string, error) {
	generate := exec.CommandContext(ctx, executable, "generate", "--home="+home, "--no-port-probing")
	generate.Stdout = io.Discard
	generate.Stderr = io.Discard
	if err := generate.Run(); err != nil {
		return "", err
	}
	deviceID := exec.CommandContext(ctx, executable, "device-id", "--home="+home)
	deviceID.Stderr = io.Discard
	output, err := deviceID.Output()
	if err != nil || len(output) > 4096 {
		return "", errors.New("read generated Syncthing device ID")
	}
	return strings.TrimSpace(string(output)), nil
}

func readBootstrapFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read Syncthing bootstrap file: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxSyncthingBootstrapFile+1))
	if err != nil || len(contents) > maxSyncthingBootstrapFile {
		return nil, errors.New("Syncthing bootstrap file is invalid")
	}
	return contents, nil
}

func hardenGeneratedConfig(data []byte) ([]byte, []byte, error) {
	var extracted struct {
		GUI struct {
			APIKey string `xml:"apikey"`
		} `xml:"gui"`
	}
	if err := xml.Unmarshal(data, &extracted); err != nil || strings.TrimSpace(extracted.GUI.APIKey) == "" {
		return nil, nil, errors.New("generated Syncthing config has no API key")
	}
	replacements := map[string]string{
		"configuration/gui/address":                   "127.0.0.1:8384",
		"configuration/gui/apikey":                    "",
		"configuration/options/listenAddress":         "tcp://127.0.0.1:22000",
		"configuration/options/globalAnnounceEnabled": "false",
		"configuration/options/localAnnounceEnabled":  "false",
		"configuration/options/relaysEnabled":         "false",
		"configuration/options/startBrowser":          "false",
		"configuration/options/urAccepted":            "-1",
		"configuration/options/upgradeToPreReleases":  "false",
	}
	hardened, err := rewriteXMLText(data, replacements)
	if err != nil {
		return nil, nil, err
	}
	return []byte(strings.TrimSpace(extracted.GUI.APIKey)), hardened, nil
}

// HardenWSLConfig keeps the local REST credential in the service-owned WSL
// config while disabling discovery, relays, telemetry, browser startup, and
// all non-loopback GUI access. The sync listener remains reachable through the
// Windows-managed private bridge only.
func HardenWSLConfig(data []byte) ([]byte, error) {
	var extracted struct {
		GUI struct {
			APIKey string `xml:"apikey"`
		} `xml:"gui"`
	}
	if err := xml.Unmarshal(data, &extracted); err != nil || strings.TrimSpace(extracted.GUI.APIKey) == "" {
		return nil, errors.New("generated Syncthing config has no API key")
	}
	return rewriteXMLText(data, map[string]string{
		"configuration/gui/address":                   "127.0.0.1:8384",
		"configuration/options/listenAddress":         "tcp://0.0.0.0:22000",
		"configuration/options/globalAnnounceEnabled": "false",
		"configuration/options/localAnnounceEnabled":  "false",
		"configuration/options/relaysEnabled":         "false",
		"configuration/options/startBrowser":          "false",
		"configuration/options/urAccepted":            "-1",
		"configuration/options/upgradeToPreReleases":  "false",
	})
}

func rewriteXMLText(data []byte, replacements map[string]string) ([]byte, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	stack := make([]string, 0, 8)
	suppressDepth := 0
	seen := make(map[string]bool, len(replacements))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("decode Syncthing config")
		}
		switch typed := token.(type) {
		case xml.StartElement:
			stack = append(stack, typed.Name.Local)
			if err := encoder.EncodeToken(token); err != nil {
				return nil, err
			}
			path := strings.Join(stack, "/")
			if replacement, ok := replacements[path]; ok {
				seen[path] = true
				suppressDepth = len(stack)
				if replacement != "" {
					if err := encoder.EncodeToken(xml.CharData([]byte(replacement))); err != nil {
						return nil, err
					}
				}
			}
		case xml.EndElement:
			if err := encoder.EncodeToken(token); err != nil {
				return nil, err
			}
			if suppressDepth == len(stack) {
				suppressDepth = 0
			}
			stack = stack[:len(stack)-1]
		default:
			if suppressDepth == 0 {
				if err := encoder.EncodeToken(token); err != nil {
					return nil, err
				}
			}
		}
	}
	for path := range replacements {
		if !seen[path] {
			return nil, fmt.Errorf("generated Syncthing config is missing %s", path)
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
