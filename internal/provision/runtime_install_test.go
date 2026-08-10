package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWSLRuntimeInstallerValidatesAssetsBeforeCallingWSL(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "remote-docker-remote-linux-amd64")
	manifest := candidate + ".sha256"
	if err := os.WriteFile(candidate, []byte("new-linux-runtime"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, contents := range map[string]string{
		"missing manifest":    "",
		"malformed manifest":  "not-a-sha256  remote-docker-remote-linux-amd64\n",
		"mismatched manifest": strings.Repeat("0", 64) + "  remote-docker-remote-linux-amd64\n",
	} {
		t.Run(name, func(t *testing.T) {
			if contents == "" {
				_ = os.Remove(manifest)
			} else if err := os.WriteFile(manifest, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			runner := &recordingRuntimeInstallRunner{}
			installer := WSLRuntimeInstaller{Runner: runner, CandidatePath: candidate, ChecksumPath: manifest}
			if err := installer.Install(context.Background()); err == nil {
				t.Fatal("Install() accepted invalid packaged runtime assets")
			}
			if len(runner.commands) != 0 {
				t.Fatalf("WSL commands = %d, want 0", len(runner.commands))
			}
		})
	}
}

func TestWSLRuntimeInstallerUsesManagedAtomicUpdateCommand(t *testing.T) {
	root := t.TempDir()
	candidateBytes := []byte("new-linux-runtime")
	digest := sha256.Sum256(candidateBytes)
	digestText := hex.EncodeToString(digest[:])
	candidate := filepath.Join(root, "remote-docker-remote-linux-amd64")
	manifest := candidate + ".sha256"
	if err := os.WriteFile(candidate, candidateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(digestText+"  remote-docker-remote-linux-amd64\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRuntimeInstallRunner{}
	installer := WSLRuntimeInstaller{
		Runner: runner, WSLBinary: "wsl.exe", Distro: "remote-docker",
		CandidatePath: candidate, ChecksumPath: manifest,
	}

	if err := installer.Install(context.Background()); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(runner.commands))
	}
	command := runner.commands[0]
	wantPrefix := []string{"--distribution", "remote-docker", "--user", "root", "--exec", "/bin/sh", "-c"}
	if command.Binary != "wsl.exe" || len(command.Args) != 10 || !reflect.DeepEqual(command.Args[:7], wantPrefix) {
		t.Fatalf("command = %#v", command)
	}
	if command.Args[8] != "remote-docker-runtime" || command.Args[9] != digestText {
		t.Fatalf("script arguments = %#v", command.Args[8:])
	}
	if !bytes.Equal(command.Input, candidateBytes) {
		t.Fatalf("runtime input = %q", command.Input)
	}
	for _, fragment := range []string{
		"remote-docker-managed-v1", "sha256sum", "trap", "rm -f", "chmod 0755", "chown root:root", "mv -f",
		"/usr/local/bin/remote-docker-remote", "[ -L \"$target\" ]",
	} {
		if !strings.Contains(command.Args[7], fragment) {
			t.Fatalf("install script is missing %q", fragment)
		}
	}
}

func TestWSLRuntimeInstallerReportsWSLFailure(t *testing.T) {
	root := t.TempDir()
	candidateBytes := []byte("new-linux-runtime")
	digest := sha256.Sum256(candidateBytes)
	candidate := filepath.Join(root, "remote-docker-remote-linux-amd64")
	manifest := candidate + ".sha256"
	if err := os.WriteFile(candidate, candidateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(hex.EncodeToString(digest[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRuntimeInstallRunner{err: errors.New("wsl failed")}
	installer := WSLRuntimeInstaller{Runner: runner, CandidatePath: candidate, ChecksumPath: manifest}
	if err := installer.Install(context.Background()); err == nil || strings.Contains(err.Error(), "wsl failed") {
		t.Fatalf("Install() error = %v", err)
	}
}

type recordingRuntimeInstallRunner struct {
	commands []runtimeIdentityCommand
	err      error
}

func (r *recordingRuntimeInstallRunner) Run(_ context.Context, command RuntimeIdentityCommand) error {
	var input []byte
	if command.Stdin != nil {
		input, _ = io.ReadAll(command.Stdin)
	}
	r.commands = append(r.commands, runtimeIdentityCommand{
		Binary: command.Binary,
		Args:   append([]string(nil), command.Args...),
		Input:  input,
	})
	return r.err
}
