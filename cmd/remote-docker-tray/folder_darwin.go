//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c -fblocks
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>
#include <dispatch/dispatch.h>
#include <stdlib.h>

static char* remoteDockerChooseDirectory(void) {
	__block char* selected = NULL;
	void (^showPanel)(void) = ^{
		@autoreleasepool {
			NSOpenPanel* panel = [NSOpenPanel openPanel];
			[panel setTitle:@"Choose workspace directory"];
			[panel setCanChooseFiles:NO];
			[panel setCanChooseDirectories:YES];
			[panel setAllowsMultipleSelection:NO];
			[panel setCanCreateDirectories:NO];
			if ([panel runModal] == NSModalResponseOK) {
				NSURL* url = [[panel URLs] firstObject];
				if (url != nil) {
					selected = strdup([[url path] fileSystemRepresentation]);
				}
			}
		}
	};
	if ([NSThread isMainThread]) {
		showPanel();
	} else {
		dispatch_sync(dispatch_get_main_queue(), showPanel);
	}
	return selected;
}
*/
import "C"

import (
	"context"
	"errors"
	"unsafe"
)

type nativeDirectoryPicker struct{}

func (nativeDirectoryPicker) Choose(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	selected := C.remoteDockerChooseDirectory()
	if selected == nil {
		return "", errors.New("workspace selection cancelled")
	}
	defer C.free(unsafe.Pointer(selected))
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return C.GoString(selected), nil
}
