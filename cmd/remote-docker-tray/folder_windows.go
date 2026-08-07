//go:build windows

package main

import (
	"context"
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	bifReturnOnlyFSDirs = 0x0001
	bifNewDialogStyle   = 0x0040
	coinitApartment     = 0x0002
)

var (
	shell32                  = windows.NewLazySystemDLL("shell32.dll")
	ole32                    = windows.NewLazySystemDLL("ole32.dll")
	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
)

type browseInfo struct {
	Owner       windows.Handle
	Root        uintptr
	DisplayName *uint16
	Title       *uint16
	Flags       uint32
	Callback    uintptr
	Parameter   uintptr
	Image       int32
}

type nativeDirectoryPicker struct{}

func (nativeDirectoryPicker) Choose(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hresult, _, _ := procCoInitializeEx.Call(0, coinitApartment)
	if hresult == 0 || hresult == 1 {
		defer procCoUninitialize.Call()
	}
	title, err := windows.UTF16PtrFromString("Choose workspace directory")
	if err != nil {
		return "", errors.New("prepare workspace directory picker")
	}
	displayName := make([]uint16, windows.MAX_PATH)
	info := browseInfo{
		DisplayName: &displayName[0],
		Title:       title,
		Flags:       bifReturnOnlyFSDirs | bifNewDialogStyle,
	}
	itemID, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&info)))
	if itemID == 0 {
		return "", errors.New("workspace selection cancelled")
	}
	defer procCoTaskMemFree.Call(itemID)

	path := make([]uint16, windows.MAX_PATH)
	ok, _, _ := procSHGetPathFromIDListW.Call(itemID, uintptr(unsafe.Pointer(&path[0])))
	if ok == 0 {
		return "", errors.New("read selected workspace directory")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return windows.UTF16ToString(path), nil
}
