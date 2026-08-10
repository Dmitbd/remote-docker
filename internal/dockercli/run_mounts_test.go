package dockercli

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestAnalyzeDockerRunBindMounts(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "short volume", args: []string{"run", "-v", "/Users/demo/project:/app", "alpine"}, want: []string{"/Users/demo/project"}},
		{name: "short equals read only", args: []string{"run", "-v=./src:/app:ro", "alpine"}, want: []string{"./src"}},
		{name: "long volume", args: []string{"run", "--volume", "../shared:/shared", "alpine"}, want: []string{"../shared"}},
		{name: "long volume equals", args: []string{"run", "--volume=/Users/demo/My Project:/app", "alpine"}, want: []string{"/Users/demo/My Project"}},
		{name: "mount bind", args: []string{"run", "--mount", "type=bind,src=/Users/demo/src,dst=/app,readonly", "alpine"}, want: []string{"/Users/demo/src"}},
		{name: "mount source target", args: []string{"run", "--mount=type=bind,source=./src,target=/app", "alpine"}, want: []string{"./src"}},
		{name: "named and anonymous volumes", args: []string{"run", "-v", "cache:/cache", "-v", "/anonymous", "--mount", "type=volume,src=data,dst=/data", "alpine"}},
		{name: "multiple binds", args: []string{"run", "-v", "./a:/a", "--mount", "type=bind,src=../b,dst=/b", "alpine"}, want: []string{"./a", "../b"}},
		{name: "container command is not parsed", args: []string{"run", "alpine", "--", "-v", "/host:/container"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis, err := Analyze(context.Background(), Invocation{Args: tt.args}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(analysis.BindSources, tt.want) {
				t.Fatalf("BindSources = %#v, want %#v", analysis.BindSources, tt.want)
			}
			if !analysis.NeedsEngine {
				t.Fatal("docker run did not require the engine")
			}
		})
	}
}

func FuzzRunMountParser(f *testing.F) {
	for _, args := range [][]string{
		{"run", "-v", "./src:/app", "alpine"},
		{"run", "--mount=type=bind,src=/tmp/a,dst=/a", "alpine"},
		{"run", "-p", "8080:80/tcp", "alpine"},
		{"run", "--", "alpine", "-v", "x:y"},
	} {
		f.Add(joinFuzzArgs(args))
	}
	f.Fuzz(func(t *testing.T, encoded string) {
		args := splitFuzzArgs(encoded)
		_, _ = Analyze(context.Background(), Invocation{Args: args}, nil)
	})
}

func joinFuzzArgs(args []string) string { return strings.Join(args, "\x00") }

func splitFuzzArgs(encoded string) []string {
	if encoded == "" {
		return nil
	}
	return strings.Split(encoded, "\x00")
}
