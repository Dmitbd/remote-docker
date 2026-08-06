package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const version = "dev"

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Method  string `json:"method"`
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
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "health":
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "ok", "version": version})
	case "rpc":
		os.Exit(runRPC(os.Stdin, os.Stdout, os.Stderr))
	default:
		os.Exit(2)
	}
}

func runRPC(input io.Reader, output, errorOutput io.Writer) int {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		var incoming request
		if err := json.Unmarshal(scanner.Bytes(), &incoming); err != nil {
			_ = encoder.Encode(response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		outgoing := response{JSONRPC: "2.0", ID: incoming.ID}
		if incoming.JSONRPC != "2.0" {
			outgoing.Error = &rpcError{Code: -32600, Message: "invalid request"}
		} else if incoming.Method != "health" {
			outgoing.Error = &rpcError{Code: -32601, Message: "method not available in this build stage"}
		} else {
			outgoing.Result = map[string]any{"status": "ok", "version": version}
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
