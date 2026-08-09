// Package processowner establishes the operating-system boundary used to
// contain every process spawned by one manually launched desktop app.
package processowner

type Owner interface {
	Active() bool
}

func AttachCurrentProcess() (Owner, error) {
	return attachCurrentProcess()
}
