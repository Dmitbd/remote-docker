//go:build windows

package processowner

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsOwner struct {
	job windows.Handle
}

func (o *windowsOwner) Active() bool { return o != nil && o.job != 0 }

func attachCurrentProcess() (Owner, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	// Keep the only Job handle open for the desktop process lifetime. On a
	// crash or normal process exit Windows closes it and kills remaining owned
	// children. Graceful shutdown stops those children before process exit.
	return &windowsOwner{job: job}, nil
}
