package deploy_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 部署范例的安全基线回归测试：systemd 不以 root 运行且启用沙箱，
// docker-compose 不给 Postgres 默认密码。

func readDeployFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSystemdExamplesRunAsDedicatedUserWithSandbox(t *testing.T) {
	units, err := filepath.Glob("twilight*.service")
	if err != nil || len(units) == 0 {
		t.Fatalf("no unit examples found: %v", err)
	}
	for _, unit := range units {
		content := readDeployFile(t, unit)
		if regexp.MustCompile(`(?m)^(User|Group)=root\s*$`).MatchString(content) || strings.Contains(content, "/root/") {
			t.Fatalf("%s must not run as root or live under /root:\n%s", unit, content)
		}
		for _, directive := range []string{"NoNewPrivileges=true", "ProtectSystem=strict", "ReadWritePaths=", "PrivateTmp=true"} {
			if !strings.Contains(content, directive) {
				t.Fatalf("%s missing %s", unit, directive)
			}
		}
	}
}

func TestSetupSystemdDefaultsToDedicatedUserWithSandbox(t *testing.T) {
	script := readDeployFile(t, "setup-systemd.sh")
	if strings.Contains(script, "TWILIGHT_SYSTEMD_USER:-root") {
		t.Fatal("setup-systemd.sh must not default the service user to root")
	}
	for _, want := range []string{"NoNewPrivileges=true", "ProtectSystem=strict", "ReadWritePaths=$rw_paths"} {
		if !strings.Contains(script, want) {
			t.Fatalf("setup-systemd.sh missing %s", want)
		}
	}
	// 每个生成的 unit 都要带沙箱块。
	if got := strings.Count(script, "$(hardening_block "); got < 3 {
		t.Fatalf("hardening block used %d times, want api/worker/webui units", got)
	}
}
