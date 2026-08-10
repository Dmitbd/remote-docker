//go:build integration

package credentials

import (
	"fmt"
	"testing"
	"time"
)

func TestKeyringStoreIntegration(t *testing.T) {
	namespace := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	testStoreContract(t, NewKeyringStore(), namespace)
}
