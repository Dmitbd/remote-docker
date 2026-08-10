package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "wait-for-interrupt" {
		waitForInterrupt()
		return
	}

	for index, argument := range os.Args[1:] {
		fmt.Printf("arg[%d]=%q\n", index, argument)
	}
	fmt.Printf("env=%s\n", os.Getenv("REMOTE_DOCKER_TEST_ENV"))

	stdin, _ := bufio.NewReader(os.Stdin).ReadString(0)
	fmt.Printf("stdin=%s\n", stdin)
	fmt.Fprintln(os.Stderr, "helper stderr")
	os.Exit(23)
}

func waitForInterrupt() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	fmt.Println("ready")
	<-signals
	fmt.Println("interrupted")
}
