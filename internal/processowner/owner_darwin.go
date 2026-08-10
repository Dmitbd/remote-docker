//go:build darwin

package processowner

import "syscall"

type processGroupOwner struct{ pgid int }

func (o processGroupOwner) Active() bool { return o.pgid > 0 }

func attachCurrentProcess() (Owner, error) {
	pid := syscall.Getpid()
	if err := syscall.Setpgid(0, pid); err != nil && err != syscall.EPERM {
		return nil, err
	}
	return processGroupOwner{pgid: pid}, nil
}
