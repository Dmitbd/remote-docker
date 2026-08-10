package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestManagedDockerContainersStopEveryRunningContainerThroughSocketAPI(t *testing.T) {
	transport := &recordingDockerTransport{}
	client := &http.Client{Transport: transport}
	stopper := managedDockerContainers{client: client}

	if err := stopper.StopContainers(context.Background()); err != nil {
		t.Fatalf("StopContainers() error = %v", err)
	}
	want := []string{
		"GET http://docker/containers/json?all=0",
		"POST http://docker/containers/0123456789abcdef/stop?t=10",
		"POST http://docker/containers/fedcba9876543210/stop?t=10",
	}
	if len(transport.requests) != len(want) {
		t.Fatalf("requests = %v, want %v", transport.requests, want)
	}
	for index := range want {
		if transport.requests[index] != want[index] {
			t.Fatalf("request[%d] = %q, want %q", index, transport.requests[index], want[index])
		}
	}
}

type recordingDockerTransport struct {
	requests []string
}

func (t *recordingDockerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, request.Method+" "+request.URL.String())
	body := ""
	if request.Method == http.MethodGet {
		body = `[{"Id":"0123456789abcdef"},{"Id":"fedcba9876543210"}]`
	}
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    request,
	}, nil
}
