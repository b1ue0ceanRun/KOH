package koh

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Exercises the real KingFile reader against a throwaway alpine container.
// Opt in with KOH_TEST_DOCKER=1 (needs a Docker daemon and alpine:latest).
func TestObserveHillDocker(t *testing.T) {
	if os.Getenv("KOH_TEST_DOCKER") != "1" {
		t.Skip("KOH_TEST_DOCKER not set")
	}
	const name = "koh-test-hill"
	exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "alpine:latest", "sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("start container: %v %s", err, out)
	}
	defer exec.Command("docker", "rm", "-f", name).Run()
	ctx := context.Background()
	sh := func(script string) {
		if out, err := exec.Command("docker", "exec", name, "sh", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", script, err, out)
		}
	}
	h := tickHill{code: "h", os: "linux", container: name, machineOK: true, healthCmd: "true"}

	if _, err := observeHill(ctx, h); err == nil {
		t.Fatal("missing KingFile must mean no owner")
	}

	flag := NewPlayerFlag()
	if err := plantFile(ctx, name, "/root/king.txt", flag); err != nil {
		t.Fatal(err)
	}
	// A holder locking the file down must not blind the platform (reads as root).
	sh("chmod 000 /root/king.txt")
	got, err := observeHill(ctx, h)
	if err != nil || strings.TrimSpace(got) != flag {
		t.Fatalf("read KingFile: %q %v", got, err)
	}

	broken := h
	broken.healthCmd = "test -f /srv/app-ok"
	if _, err := observeHill(ctx, broken); err == nil {
		t.Fatal("failed health check must mean no owner")
	}

	sh("head -c 70000 /dev/zero | tr '\\0' ' ' > /root/king.txt")
	if _, err := observeHill(ctx, h); err == nil {
		t.Fatal("oversized KingFile must be rejected")
	}

	if err := plantFile(ctx, name, "/var/www/secret/flag.txt", "flag{planted}"); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("docker", "exec", name, "cat", "/var/www/secret/flag.txt").Output()
	if strings.TrimSpace(string(out)) != "flag{planted}" {
		t.Fatalf("planted content %q", out)
	}

	exec.Command("docker", "stop", "-t", "0", name).Run()
	if _, err := observeHill(ctx, h); err == nil {
		t.Fatal("stopped container must mean no owner")
	}
}
