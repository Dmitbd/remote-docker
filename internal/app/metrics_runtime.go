package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/metrics"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
)

type sshRemoteMetrics struct {
	store         config.Store
	sshConfigPath string
	sshBinary     string
	run           func(context.Context, sshtransport.Command) error
}

func (c sshRemoteMetrics) SampleRemote(ctx context.Context) (metrics.RemoteSample, error) {
	cfg, err := loadAgentConfig(c.store)
	if err != nil || cfg.ActiveDevice == "" {
		return metrics.RemoteSample{}, errors.New("managed metrics device is unavailable")
	}
	if _, ok := cfg.Devices[cfg.ActiveDevice]; !ok {
		return metrics.RemoteSample{}, errors.New("managed metrics device is unavailable")
	}
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "metrics.sample"})
	if err != nil {
		return metrics.RemoteSample{}, errors.New("encode managed metrics RPC")
	}
	binary := c.sshBinary
	if binary == "" {
		binary = "ssh"
	}
	var output bytes.Buffer
	command := sshtransport.Command{
		Binary: binary,
		Args: []string{
			"-F", c.sshConfigPath, "remote-docker-device-" + cfg.ActiveDevice,
			"remote-docker-remote", "rpc",
		},
		Stdin: bytes.NewReader(append(request, '\n')), Stdout: &output, Stderr: io.Discard,
	}
	run := c.run
	if run == nil {
		run = runSSHCommand
	}
	if err := run(ctx, command); err != nil {
		return metrics.RemoteSample{}, errors.New("managed metrics RPC failed")
	}
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(&output, 64<<10)).Decode(&response); err != nil ||
		response.JSONRPC != "2.0" || response.ID != 1 || len(response.Error) != 0 || len(response.Result) == 0 {
		return metrics.RemoteSample{}, errors.New("managed metrics RPC was not acknowledged")
	}
	var result metrics.RemoteSample
	decoder := json.NewDecoder(bytes.NewReader(response.Result))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return metrics.RemoteSample{}, errors.New("managed metrics RPC returned invalid result")
	}
	return result, nil
}

var _ metrics.RemoteSampler = sshRemoteMetrics{}
