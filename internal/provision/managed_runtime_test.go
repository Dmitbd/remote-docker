package provision

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

func TestManagedWSLRuntimePreparesIdentityAfterInstallingRuntime(t *testing.T) {
	var calls []string
	runtime := ManagedWSLRuntime{
		Installer: &recordingManagedRuntimeInstaller{sequence: &calls},
		Identity:  &recordingManagedIdentityPreparer{sequence: &calls},
	}
	if err := runtime.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"install", "identity"}) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestManagedWSLRuntimeDoesNotPrepareIdentityAfterInstallFailure(t *testing.T) {
	var calls []string
	runtime := ManagedWSLRuntime{
		Installer: &recordingManagedRuntimeInstaller{sequence: &calls, failures: 1},
		Identity:  &recordingManagedIdentityPreparer{sequence: &calls},
	}
	if err := runtime.Prepare(context.Background()); err == nil {
		t.Fatal("Prepare() accepted a runtime installation failure")
	}
	if !reflect.DeepEqual(calls, []string{"install"}) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestManagedWSLRuntimeRetriesInstallThenKeepsRecoveringIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []string
	installer := &recordingManagedRuntimeInstaller{sequence: &calls, failures: 1}
	identity := &recordingManagedIdentityPreparer{sequence: &calls, onCall: func(call int) {
		if call == 2 {
			cancel()
		}
	}}
	runtime := ManagedWSLRuntime{Installer: installer, Identity: identity}
	if err := runtime.Run(ctx, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if installer.calls != 2 || identity.calls != 2 {
		t.Fatalf("installer calls = %d, identity calls = %d", installer.calls, identity.calls)
	}
	if !reflect.DeepEqual(calls, []string{"install", "install", "identity", "identity"}) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestNewManagedWSLRuntimeUsesAssetsNextToInstalledAgent(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "Remote Docker", "RemoteDockerAgent.exe")
	runtime, err := NewManagedWSLRuntime(executable, credentials.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewManagedWSLRuntime() error = %v", err)
	}
	installer, ok := runtime.Installer.(WSLRuntimeInstaller)
	if !ok {
		t.Fatalf("installer type = %T", runtime.Installer)
	}
	wantCandidate := filepath.Join(filepath.Dir(executable), "assets", "remote-docker-remote-linux-amd64")
	if installer.CandidatePath != wantCandidate || installer.ChecksumPath != wantCandidate+".sha256" {
		t.Fatalf("installer paths = %q, %q", installer.CandidatePath, installer.ChecksumPath)
	}
}

type recordingManagedRuntimeInstaller struct {
	sequence *[]string
	failures int
	calls    int
}

func (i *recordingManagedRuntimeInstaller) Install(context.Context) error {
	i.calls++
	*i.sequence = append(*i.sequence, "install")
	if i.calls <= i.failures {
		return errors.New("install failed")
	}
	return nil
}

type recordingManagedIdentityPreparer struct {
	sequence *[]string
	calls    int
	onCall   func(int)
}

func (p *recordingManagedIdentityPreparer) Prepare(context.Context) error {
	p.calls++
	*p.sequence = append(*p.sequence, "identity")
	if p.onCall != nil {
		p.onCall(p.calls)
	}
	return nil
}
