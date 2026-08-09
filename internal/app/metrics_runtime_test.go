package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/metrics"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
)

func TestSSHRemoteMetricsUsesOnePinnedReadOnlyRPC(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "pc-1",
		Devices: map[string]config.Device{"pc-1": {Address: "192.168.1.20"}},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	client := sshRemoteMetrics{
		store: store, sshConfigPath: filepath.Join(root, "ssh_config"),
		run: func(_ context.Context, command sshtransport.Command) error {
			if !reflect.DeepEqual(command.Args, []string{
				"-F", filepath.Join(root, "ssh_config"), "remote-docker-device-pc-1", "remote-docker-remote", "rpc",
			}) {
				t.Fatalf("SSH args = %#v", command.Args)
			}
			var request struct{ Method string `json:"method"` }
			if err := json.NewDecoder(command.Stdin).Decode(&request); err != nil || request.Method != "metrics.sample" {
				t.Fatalf("RPC request = %#v, %v", request, err)
			}
			return json.NewEncoder(command.Stdout).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"result": metrics.RemoteSample{DockerContainers: metrics.Available(4)},
			})
		},
	}
	sample, err := client.SampleRemote(context.Background())
	if err != nil || !sample.DockerContainers.Available || sample.DockerContainers.Value != 4 {
		t.Fatalf("SampleRemote() = %#v, %v", sample, err)
	}
}
