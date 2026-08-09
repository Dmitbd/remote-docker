//go:build !darwin && !windows

package processowner

type inertOwner struct{}

func (inertOwner) Active() bool { return true }

func attachCurrentProcess() (Owner, error) { return inertOwner{}, nil }
