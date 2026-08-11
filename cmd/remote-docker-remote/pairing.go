package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const managedPairingMarker = "remote-docker-device="

type pairingRuntime struct {
	HostPublicKeyPath  string
	AuthorizedKeysPath string
	SyncthingDeviceID  func(context.Context) (string, error)
}

type pairingDeviceInfo struct {
	SSHHostPublicKey  string `json:"ssh_host_public_key"`
	SyncthingDeviceID string `json:"syncthing_device_id"`
}

func defaultPairingRuntime() pairingRuntime {
	return pairingRuntime{
		HostPublicKeyPath:  "/etc/remote-docker/ssh_host_ed25519_key.pub",
		AuthorizedKeysPath: "/var/lib/remote-docker/authorized_keys",
		SyncthingDeviceID:  readSyncthingDeviceID,
	}
}

func runPairingInstall(ctx context.Context, runtime pairingRuntime, args []string, input io.Reader, output, errorOutput io.Writer) int {
	deviceID, ok := pairingDeviceArgument(args)
	if !ok || runtime.SyncthingDeviceID == nil {
		fmt.Fprintln(errorOutput, "invalid managed pairing request")
		return 2
	}
	rawKey, err := io.ReadAll(io.LimitReader(input, (16<<10)+1))
	if err != nil || len(rawKey) > 16<<10 {
		fmt.Fprintln(errorOutput, "invalid managed pairing key")
		return 1
	}
	key, err := canonicalEd25519Key(rawKey)
	if err != nil {
		fmt.Fprintln(errorOutput, "invalid managed pairing key")
		return 1
	}
	existing, err := os.ReadFile(runtime.AuthorizedKeysPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(errorOutput, "cannot read managed pairing key")
		return 1
	}
	if len(existing) > 0 {
		managedDeviceID, managed := managedPairingDeviceID(existing)
		if !managed || managedDeviceID != deviceID {
			fmt.Fprintln(errorOutput, "connection limit reached; forget the trusted device first")
			return 1
		}
	}
	hostRaw, err := os.ReadFile(runtime.HostPublicKeyPath)
	if err != nil {
		fmt.Fprintln(errorOutput, "managed SSH host identity is unavailable")
		return 1
	}
	hostKey, err := canonicalEd25519Key(hostRaw)
	if err != nil {
		fmt.Fprintln(errorOutput, "managed SSH host identity is invalid")
		return 1
	}
	syncthingID, err := runtime.SyncthingDeviceID(ctx)
	if err != nil || strings.TrimSpace(syncthingID) == "" {
		fmt.Fprintln(errorOutput, "managed Syncthing identity is unavailable")
		return 1
	}
	managed := []byte(key + " " + managedPairingMarker + deviceID + "\n")
	if err := writeManagedAuthorization(runtime.AuthorizedKeysPath, managed); err != nil {
		fmt.Fprintln(errorOutput, "cannot install managed pairing key")
		return 1
	}
	if err := json.NewEncoder(output).Encode(pairingDeviceInfo{
		SSHHostPublicKey: hostKey, SyncthingDeviceID: strings.TrimSpace(syncthingID),
	}); err != nil {
		fmt.Fprintln(errorOutput, "cannot encode managed pairing result")
		return 1
	}
	return 0
}

func runPairingRevoke(runtime pairingRuntime, args []string, errorOutput io.Writer) int {
	deviceID, ok := pairingDeviceArgument(args)
	if !ok {
		fmt.Fprintln(errorOutput, "invalid managed pairing request")
		return 2
	}
	contents, err := os.ReadFile(runtime.AuthorizedKeysPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(errorOutput, "cannot read managed pairing key")
		return 1
	}
	if len(contents) > 0 {
		managedDeviceID, managed := managedPairingDeviceID(contents)
		if !managed || managedDeviceID != deviceID {
			fmt.Fprintln(errorOutput, "managed pairing belongs to another device")
			return 1
		}
	}
	if err := writeManagedAuthorization(runtime.AuthorizedKeysPath, nil); err != nil {
		fmt.Fprintln(errorOutput, "cannot revoke managed pairing key")
		return 1
	}
	return 0
}

func runPairingRevokeCommand(
	ctx context.Context,
	runtime pairingRuntime,
	syncRuntime remoteSyncRuntime,
	args []string,
	errorOutput io.Writer,
) int {
	deviceID, ok := pairingDeviceArgument(args)
	if !ok {
		fmt.Fprintln(errorOutput, "invalid managed pairing request")
		return 2
	}
	contents, err := os.ReadFile(runtime.AuthorizedKeysPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return runPairingRevoke(runtime, args, errorOutput)
	}
	if len(contents) == 0 {
		return runPairingRevoke(runtime, args, errorOutput)
	}
	managedDeviceID, managed := managedPairingDeviceID(contents)
	if !managed || managedDeviceID != deviceID {
		return runPairingRevoke(runtime, args, errorOutput)
	}
	if syncRuntime != nil {
		if err := syncRuntime.Revoke(ctx, deviceID); err != nil {
			fmt.Fprintln(errorOutput, "cannot revoke managed Syncthing trust")
			return 1
		}
	}
	return runPairingRevoke(runtime, args, errorOutput)
}

func managedPairingDeviceID(contents []byte) (string, bool) {
	key, comment, options, rest, err := ssh.ParseAuthorizedKey(contents)
	if err != nil || key.Type() != ssh.KeyAlgoED25519 || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return "", false
	}
	deviceID, ok := strings.CutPrefix(comment, managedPairingMarker)
	if !ok || managedPairingMarker+deviceID != comment {
		return "", false
	}
	if _, valid := pairingDeviceArgument([]string{"--device", deviceID}); !valid {
		return "", false
	}
	return deviceID, true
}

func pairingDeviceArgument(args []string) (string, bool) {
	if len(args) != 2 || args[0] != "--device" || args[1] == "" || len(args[1]) > 128 {
		return "", false
	}
	for _, character := range args[1] {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return "", false
	}
	return args[1], true
}

func canonicalEd25519Key(raw []byte) (string, error) {
	publicKey, _, _, rest, err := ssh.ParseAuthorizedKey(bytes.TrimSpace(raw))
	if err != nil || publicKey.Type() != ssh.KeyAlgoED25519 || len(bytes.TrimSpace(rest)) != 0 {
		return "", errors.New("invalid Ed25519 public key")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))), nil
}

func writeManagedAuthorization(path string, contents []byte) (returnErr error) {
	if !filepath.IsAbs(path) {
		return errors.New("managed authorization path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".authorized_keys.tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
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

func readSyncthingDeviceID(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := "/var/lib/remote-docker/syncthing/device-id"
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", errors.New("managed Syncthing device ID is unavailable")
	}
	output, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(output)) == "" {
		return "", errors.New("managed Syncthing device ID is unavailable")
	}
	return strings.TrimSpace(string(output)), nil
}
