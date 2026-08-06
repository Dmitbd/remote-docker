package syncer

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWaitReadyRequiresIdleCompleteFolderAndConnectedDevice(t *testing.T) {
	source := &scriptedStatusSource{
		folders: []FolderStatus{
			{State: "syncing", NeedTotalItems: 4},
			{State: "idle", NeedTotalItems: 0, PullErrors: 0},
		},
		connections: []map[string]ConnectionStatus{
			{"REMOTE": {Connected: false}},
			{"REMOTE": {Connected: true}},
		},
	}
	if err := WaitReady(context.Background(), source, "folder-1", "REMOTE", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if source.polls != 2 {
		t.Fatalf("polls = %d, want 2", source.polls)
	}
}

func TestWaitReadyTimeoutIsActionableAndDoesNotLeakSecrets(t *testing.T) {
	source := &scriptedStatusSource{
		folders: []FolderStatus{{State: "syncing", NeedTotalItems: 7, PullErrors: 2}},
		connections: []map[string]ConnectionStatus{
			{"REMOTE": {Connected: false}},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	err := WaitReady(ctx, source, "folder-secret-safe", "REMOTE", time.Millisecond)
	if err == nil {
		t.Fatal("WaitReady() succeeded")
	}
	message := err.Error()
	for _, wanted := range []string{"folder-secret-safe", "state=syncing", "need=7", "pull_errors=2", "connected=false"} {
		if !strings.Contains(message, wanted) {
			t.Fatalf("error %q does not contain %q", message, wanted)
		}
	}
	if strings.Contains(message, "api-key") {
		t.Fatalf("error leaked a secret: %q", message)
	}
}

type scriptedStatusSource struct {
	folders     []FolderStatus
	connections []map[string]ConnectionStatus
	polls       int
}

func (s *scriptedStatusSource) FolderStatus(context.Context, string) (FolderStatus, error) {
	index := s.polls
	if index >= len(s.folders) {
		index = len(s.folders) - 1
	}
	return s.folders[index], nil
}

func (s *scriptedStatusSource) Connections(context.Context) (map[string]ConnectionStatus, error) {
	index := s.polls
	if index >= len(s.connections) {
		index = len(s.connections) - 1
	}
	s.polls++
	return s.connections[index], nil
}
