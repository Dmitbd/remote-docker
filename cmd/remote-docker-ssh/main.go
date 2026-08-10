//go:build darwin

package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/Dmitbd/remote-docker/internal/sshtransport"
)

func main() {
	command, err := sshtransport.DockerSSHInvocation(
		os.Getenv(sshtransport.DockerSSHConfigEnvironment), os.Args[1:],
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker ssh adapter:", err)
		os.Exit(2)
	}
	argv := append([]string{command.Binary}, command.Args...)
	if err := syscall.Exec(command.Binary, argv, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker ssh adapter:", err)
		os.Exit(1)
	}
}
