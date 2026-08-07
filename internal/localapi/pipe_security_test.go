package localapi

import (
	"strings"
	"testing"
)

func TestAgentWindowsPipeSecurityDescriptorAllowsOnlyCurrentOwner(t *testing.T) {
	descriptor := ownerOnlyPipeSecurityDescriptor()
	if descriptor != "D:P(A;;GA;;;OW)" {
		t.Fatalf("pipe security descriptor = %q, want owner-only protected DACL", descriptor)
	}
	for _, forbidden := range []string{";;;SY", ";;;BA", ";;;WD", ";;;AU"} {
		if strings.Contains(descriptor, forbidden) {
			t.Fatalf("pipe security descriptor grants forbidden principal %q: %s", forbidden, descriptor)
		}
	}
}
