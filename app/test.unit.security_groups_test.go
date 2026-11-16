package app

import (
	"testing"

	testkit "github.com/slidebolt/sb-testkit"
)

func TestSecurityGroupContract(t *testing.T) {
	testkit.VerifyPluginSecurityGroups(t, New().Hello())
}
