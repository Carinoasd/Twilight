package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/prejudice-studio/twilight/internal/config"
	"github.com/prejudice-studio/twilight/internal/store"
)

func runTwoFactorReset(args []string) error {
	fs := flag.NewFlagSet("reset-2fa", flag.ContinueOnError)
	uid := fs.Int64("uid", 0, "UID of the verified account owner")
	confirm := fs.String("confirm", "", "must equal RESET_2FA_<uid>")
	configFile := fs.String("config", "", "config.toml path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *uid <= 0 || *confirm != fmt.Sprintf("RESET_2FA_%d", *uid) {
		return fmt.Errorf("requires --uid and --confirm RESET_2FA_<uid>; verify the account owner before recovery")
	}
	path, err := runtimeConfigPath(*configFile)
	if err != nil {
		return err
	}
	cfg, err := config.NewReader(path).ReadLocked()
	if err != nil {
		return err
	}
	st, err := openStore(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	// Record the attempt before changing authentication. An audit failure blocks recovery.
	if err = st.AddAuditLog(store.AuditLog{Username: "console", Action: "two_factor_reset_requested", Category: "admin", Source: "console", TargetUID: *uid}, 0); err != nil {
		return err
	}
	if err = st.ResetTwoFactor(context.Background(), *uid); err != nil {
		return err
	}
	if err = st.AddAuditLog(store.AuditLog{Username: "console", Action: "two_factor_reset", Category: "admin", Source: "console", TargetUID: *uid}, 0); err != nil {
		return fmt.Errorf("2FA reset succeeded; final audit failed: %w", err)
	}
	fmt.Printf("2FA reset for UID %d; sessions and login requests revoked.\n", *uid)
	return nil
}
