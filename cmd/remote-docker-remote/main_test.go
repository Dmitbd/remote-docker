package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRPCHealthAndMethodAllowlist(t *testing.T) {
	input := strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"health\"}\n" +
			"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"shell.exec\"}\n",
	)
	var output bytes.Buffer
	var stderr bytes.Buffer

	if code := runRPC(input, &output, &stderr); code != 0 {
		t.Fatalf("runRPC() code = %d, stderr = %s", code, &stderr)
	}
	decoder := json.NewDecoder(&output)
	var health response
	if err := decoder.Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health.Error != nil || health.Result["status"] != "ok" {
		t.Fatalf("health response = %#v", health)
	}
	var rejected response
	if err := decoder.Decode(&rejected); err != nil {
		t.Fatalf("decode rejected response: %v", err)
	}
	if rejected.Error == nil || rejected.Error.Code != -32601 {
		t.Fatalf("rejected response = %#v", rejected)
	}
}

func TestRPCDoesNotEchoMalformedInput(t *testing.T) {
	secret := "private-token-value"
	input := strings.NewReader("{not-json-" + secret + "}\n")
	var output bytes.Buffer

	if code := runRPC(input, &output, &bytes.Buffer{}); code != 0 {
		t.Fatalf("runRPC() code = %d", code)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("parse response leaked request content: %s", &output)
	}
}
