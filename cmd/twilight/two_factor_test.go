package main

import (
	"strings"
	"testing"
)

func TestTwoFactorResetRequiresExactConfirmationBeforeConfiguration(t *testing.T) {
	// An empty working directory has no configuration or database. Invalid
	// confirmation must fail before any configuration read or store access.
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{},
		{"--uid", "0", "--confirm", "RESET_2FA_0"},
		{"--uid", "7"},
		{"--uid", "7", "--confirm", "RESET_2FA_8"},
	} {
		err := runTwoFactorReset(args)
		if err == nil || !strings.Contains(err.Error(), "requires --uid and --confirm") {
			t.Fatalf("invalid confirmation reached configuration or recovery: %v", err)
		}
	}
}
