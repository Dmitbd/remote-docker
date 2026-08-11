package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const version = "dev"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
	Error   *rpcError      `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "health":
		if len(os.Args) != 2 {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "ok", "version": version})
	case "rpc":
		if len(os.Args) != 2 {
			os.Exit(2)
		}
		os.Exit(runRPC(os.Stdin, os.Stdout, os.Stderr))
	case "pairing-install":
		os.Exit(runPairingInstall(context.Background(), defaultPairingRuntime(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "pairing-revoke":
		os.Exit(runPairingRevokeCommand(
			context.Background(), defaultPairingRuntime(), defaultRemoteSyncRuntime(), os.Args[2:], os.Stderr,
		))
	case "runtime-prepare":
		if len(os.Args) != 2 {
			os.Exit(2)
		}
		os.Exit(runRuntimePrepare(context.Background(), os.Stdin, os.Stderr))
	case "runtime-status":
		if len(os.Args) != 2 {
			os.Exit(2)
		}
		os.Exit(runRuntimeStatus(context.Background()))
	case "runtime-identity-state":
		if len(os.Args) != 2 {
			os.Exit(2)
		}
		os.Exit(runRuntimeIdentityState(os.Stdout))
	default:
		os.Exit(2)
	}
}

func runRPC(input io.Reader, output, errorOutput io.Writer) int {
	marker := newProcessPresenceMarker(managedPresenceMarkerRoot, os.Getpid())
	return runRPCWithPresenceMarker(input, output, errorOutput, defaultPairingRuntime(), remoteSystemOperations{}, defaultRemoteSyncRuntime(), marker)
}

func runRPCWithRuntime(input io.Reader, output, errorOutput io.Writer, pairingRuntime pairingRuntime) int {
	return runRPCWithAllOperations(input, output, errorOutput, pairingRuntime, remoteSystemOperations{}, nil)
}

func runRPCWithOperations(
	input io.Reader,
	output, errorOutput io.Writer,
	pairingRuntime pairingRuntime,
	diagnosticsRuntime remoteDiagnosticsRuntime,
) int {
	return runRPCWithAllOperations(input, output, errorOutput, pairingRuntime, diagnosticsRuntime, nil)
}

func runRPCWithAllOperations(
	input io.Reader,
	output, errorOutput io.Writer,
	pairingRuntime pairingRuntime,
	diagnosticsRuntime remoteDiagnosticsRuntime,
	syncRuntime remoteSyncRuntime,
) int {
	return runRPCWithPresenceMarker(input, output, errorOutput, pairingRuntime, diagnosticsRuntime, syncRuntime, nil)
}

func runRPCWithPresenceMarker(
	input io.Reader,
	output, errorOutput io.Writer,
	pairingRuntime pairingRuntime,
	diagnosticsRuntime remoteDiagnosticsRuntime,
	syncRuntime remoteSyncRuntime,
	marker rpcPresenceMarker,
) int {
	if marker != nil {
		defer marker.Deactivate()
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(output)
	presence := newPresenceManager(presenceOptions{Ready: func(ctx context.Context) (bool, bool) {
		if diagnosticsRuntime == nil {
			return false, false
		}
		observation, err := diagnosticsRuntime.Observe(ctx)
		if err != nil {
			return false, false
		}
		return observation.DockerSocket, observation.SyncthingService
	}})
	for scanner.Scan() {
		var incoming request
		if err := json.Unmarshal(scanner.Bytes(), &incoming); err != nil {
			_ = encoder.Encode(response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		outgoing := response{JSONRPC: "2.0", ID: incoming.ID}
		if incoming.JSONRPC != "2.0" {
			outgoing.Error = &rpcError{Code: -32600, Message: "invalid request"}
		} else if incoming.Method == "health" {
			outgoing.Result = map[string]any{"status": "ok", "version": version}
		} else if incoming.Method == "pairing.revoke" {
			var params struct {
				DeviceID string `json:"device_id"`
			}
			decoder := json.NewDecoder(bytes.NewReader(incoming.Params))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&params); err != nil {
				outgoing.Error = &rpcError{Code: -32602, Message: "invalid params"}
			} else if code := runPairingRevokeCommand(
				context.Background(), pairingRuntime, syncRuntime, []string{"--device", params.DeviceID}, io.Discard,
			); code != 0 {
				outgoing.Error = &rpcError{Code: -32001, Message: "managed pairing revocation failed"}
			} else {
				outgoing.Result = map[string]any{"revoked": true}
			}
		} else if incoming.Method == "diagnostics.observe" && len(incoming.Params) == 0 {
			if diagnosticsRuntime == nil {
				outgoing.Error = &rpcError{Code: -32002, Message: "managed diagnostics observation failed"}
			} else {
				observation, err := diagnosticsRuntime.Observe(context.Background())
				if err != nil {
					outgoing.Error = &rpcError{Code: -32002, Message: "managed diagnostics observation failed"}
				} else {
					outgoing.Result = map[string]any{
						"wsl_running":       observation.WSLRunning,
						"systemd_target":    observation.SystemdTarget,
						"docker_socket":     observation.DockerSocket,
						"disk_available":    observation.DiskAvailable,
						"syncthing_service": observation.SyncthingService,
						"presence_active":   observation.PresenceActive,
					}
				}
			}
		} else if incoming.Method == "recovery.restart-systemd" && len(incoming.Params) == 0 {
			if diagnosticsRuntime == nil {
				outgoing.Error = &rpcError{Code: -32003, Message: "managed systemd recovery failed"}
			} else {
				if err := diagnosticsRuntime.RestartSystemdTarget(context.Background()); err != nil {
					outgoing.Error = &rpcError{Code: -32003, Message: "managed systemd recovery failed"}
				} else {
					outgoing.Result = map[string]any{"restarted": true}
				}
			}
		} else if incoming.Method == "runtime.stop-containers" && len(incoming.Params) == 0 {
			lifecycleRuntime, ok := diagnosticsRuntime.(remoteLifecycleRuntime)
			if !ok || lifecycleRuntime == nil {
				outgoing.Error = &rpcError{Code: -32005, Message: "managed container stop failed"}
			} else if err := lifecycleRuntime.StopContainers(context.Background()); err != nil {
				outgoing.Error = &rpcError{Code: -32005, Message: "managed container stop failed"}
			} else {
				outgoing.Result = map[string]any{"stopped": true}
			}
		} else if incoming.Method == "presence.hello" {
			var params presenceHelloParams
			if decodeRPCParams(incoming.Params, &params) != nil {
				outgoing.Error = &rpcError{Code: -32602, Message: "invalid params"}
			} else {
				result, helloErr := presence.Hello(context.Background(), params)
				if helloErr == nil && marker != nil {
					helloErr = marker.Activate()
					if helloErr != nil {
						_ = presence.Disconnect(context.Background(), presenceDisconnectParams{SessionID: result.SessionID, Reason: "runtime_failure"})
					}
				}
				if helloErr != nil {
					outgoing.Error = &rpcError{Code: -32007, Message: "presence session unavailable"}
				} else {
					outgoing.Result = presenceRPCResult(result)
				}
			}
		} else if incoming.Method == "presence.heartbeat" {
			var params presenceHeartbeatParams
			if decodeRPCParams(incoming.Params, &params) != nil {
				outgoing.Error = &rpcError{Code: -32602, Message: "invalid params"}
			} else if result, err := presence.Heartbeat(context.Background(), params); err != nil {
				outgoing.Error = &rpcError{Code: -32008, Message: "presence heartbeat rejected"}
			} else {
				outgoing.Result = presenceRPCResult(result)
			}
		} else if incoming.Method == "presence.disconnect" {
			var params presenceDisconnectParams
			if decodeRPCParams(incoming.Params, &params) != nil {
				outgoing.Error = &rpcError{Code: -32602, Message: "invalid params"}
			} else if err := presence.Disconnect(context.Background(), params); err != nil {
				outgoing.Error = &rpcError{Code: -32009, Message: "presence disconnect rejected"}
			} else {
				if marker != nil {
					marker.Deactivate()
				}
				outgoing.Result = map[string]any{"disconnected": true}
			}
		} else if incoming.Method == "metrics.sample" && len(incoming.Params) == 0 {
			outgoing.Result = metricsResultMap(collectManagedRuntimeMetrics(context.Background()))
		} else if incoming.Method == "sync.configure" {
			var params remoteSyncConfigureParams
			if syncRuntime == nil || decodeRPCParams(incoming.Params, &params) != nil {
				outgoing.Error = &rpcError{Code: -32602, Message: "invalid params"}
			} else if err := syncRuntime.Configure(context.Background(), params); err != nil {
				outgoing.Error = &rpcError{Code: -32004, Message: "managed sync configuration failed"}
			} else {
				outgoing.Result = map[string]any{"configured": true}
			}
		} else if incoming.Method == "sync.scan" {
			var params remoteSyncFolderParams
			if syncRuntime == nil || decodeRPCParams(incoming.Params, &params) != nil {
				outgoing.Error = &rpcError{Code: -32602, Message: "invalid params"}
			} else if err := syncRuntime.Scan(context.Background(), params.FolderID); err != nil {
				outgoing.Error = &rpcError{Code: -32005, Message: "managed sync scan failed"}
			} else {
				outgoing.Result = map[string]any{"scanned": true}
			}
		} else if incoming.Method == "sync.status" {
			var params remoteSyncStatusParams
			if syncRuntime == nil || decodeRPCParams(incoming.Params, &params) != nil {
				outgoing.Error = &rpcError{Code: -32602, Message: "invalid params"}
			} else if status, err := syncRuntime.Status(context.Background(), params.FolderID, params.DeviceID); err != nil {
				outgoing.Error = &rpcError{Code: -32006, Message: "managed sync status failed"}
			} else {
				outgoing.Result = map[string]any{
					"state": status.State, "need_total_items": status.NeedTotalItems,
					"pull_errors": status.PullErrors, "connected": status.Connected,
				}
			}
		} else {
			outgoing.Error = &rpcError{Code: -32601, Message: "method not available in this build stage"}
		}
		if err := encoder.Encode(outgoing); err != nil {
			fmt.Fprintln(errorOutput, err)
			return 1
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(errorOutput, err)
		return 1
	}
	return 0
}

func metricsResultMap(sample any) map[string]any {
	encoded, err := json.Marshal(sample)
	if err != nil {
		return map[string]any{}
	}
	result := map[string]any{}
	_ = json.Unmarshal(encoded, &result)
	return result
}

func presenceRPCResult(result presenceResult) map[string]any {
	return map[string]any{
		"session_id": result.SessionID, "server_monotonic_ms": result.ServerMonotonicMS,
		"docker_ready": result.DockerReady, "sync_ready": result.SyncReady,
		"terminal": result.Terminal, "reason": result.Reason,
	}
}

func decodeRPCParams(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("unexpected trailing params")
	}
	return nil
}
