//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AppKit
#import <AppKit/AppKit.h>

static int remoteDockerSetAccessoryActivationPolicy(void) {
	NSApplication *application = [NSApplication sharedApplication];
	return [application setActivationPolicy:NSApplicationActivationPolicyAccessory] ? 0 : 1;
}
*/
import "C"

import "errors"

func setAccessoryActivationPolicy() error {
	if C.remoteDockerSetAccessoryActivationPolicy() != 0 {
		return errors.New("configure macOS accessory application")
	}
	return nil
}
