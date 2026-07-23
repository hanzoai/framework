package framework

import (
	"crypto/rand"
	"os"
	"testing"

	"github.com/hanzoai/cloud/cek"
)

// TestMain seeds a random cek master key so the encrypted-at-rest store opens on an
// encryption-capable test build (mirrors clients/compliance, integrations, flags, git,
// venue). On a pure-Go build cek ignores it and uses the plaintext dev path.
func TestMain(m *testing.M) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	cek.SetMasterKey(k)
	os.Exit(m.Run())
}
