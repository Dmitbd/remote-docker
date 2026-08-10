//go:build !devui

package main

import "errors"

func mockBackendFromArgs(args []string, _ string) (uiBackend, bool, error) {
	if len(args) != 0 {
		return nil, false, errors.New("production UI does not accept arguments")
	}
	return nil, false, nil
}

func startupErrorMessage(error) string { return "Remote Docker UI could not start." }
