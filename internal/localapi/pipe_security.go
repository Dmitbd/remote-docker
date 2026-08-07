package localapi

// ownerOnlyPipeSecurityDescriptor is a protected DACL granting generic-all
// exclusively to the named pipe owner (OW). No SYSTEM, administrator, or
// authenticated-user ACE is present.
func ownerOnlyPipeSecurityDescriptor() string {
	return "D:P(A;;GA;;;OW)"
}
