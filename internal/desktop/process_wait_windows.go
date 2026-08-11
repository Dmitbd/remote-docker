//go:build windows

package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func WaitForNoOtherInstance(ctx context.Context, executablePath string, interval time.Duration) error {
	if !filepath.IsAbs(executablePath) {
		return errors.New("desktop executable path must be absolute")
	}
	currentUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return errors.New("read current desktop process owner")
	}
	targetPath := normalizeProcessPath(executablePath)
	return waitForNoOtherInstance(ctx, interval, func() (bool, error) {
		return hasOwnedProcessAtPath(targetPath, currentUser.User.Sid)
	})
}

func hasOwnedProcessAtPath(targetPath string, currentUser *windows.SID) (bool, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, errors.New("snapshot desktop processes")
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return false, errors.New("read desktop process snapshot")
	}
	for {
		if entry.ProcessID != uint32(os.Getpid()) && strings.EqualFold(
			windows.UTF16ToString(entry.ExeFile[:]), filepath.Base(targetPath),
		) {
			matches, err := ownedProcessMatchesPath(entry.ProcessID, targetPath, currentUser)
			if err != nil {
				return false, err
			}
			if matches {
				return true, nil
			}
		}
		err = windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return false, nil
		}
		if err != nil {
			return false, errors.New("continue desktop process snapshot")
		}
	}
}

func ownedProcessMatchesPath(processID uint32, targetPath string, currentUser *windows.SID) (bool, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, processID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("open matching desktop process")
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return false, errors.New("read matching desktop process owner")
	}
	defer token.Close()
	owner, err := token.GetTokenUser()
	if err != nil {
		return false, errors.New("resolve matching desktop process owner")
	}
	if !owner.User.Sid.Equals(currentUser) {
		return false, nil
	}
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return false, errors.New("read matching desktop process path")
	}
	return strings.EqualFold(normalizeProcessPath(windows.UTF16ToString(buffer[:size])), targetPath), nil
}

func normalizeProcessPath(path string) string {
	path = filepath.Clean(path)
	return strings.TrimPrefix(path, `\\?\`)
}
