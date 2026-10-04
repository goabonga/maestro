// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package launcher

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sandbox returns a probed launcher or skips when the host cannot
// confine — the production rule stays refusal, tests simply cannot run.
func sandbox(t *testing.T) *Launcher {
	t.Helper()
	launcher, err := New()
	if errors.Is(err, ErrUnsupported) {
		t.Skipf("host cannot confine: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return launcher
}

// runIn runs one fixture command in the sandbox and returns its output.
func runIn(t *testing.T, launcher *Launcher, spec Spec) (string, error) {
	t.Helper()
	var output bytes.Buffer
	group, err := launcher.Start(spec, nil, &output, &output)
	if err != nil {
		t.Fatal(err)
	}
	err = group.Wait()
	return strings.TrimSpace(output.String()), err
}

func TestArgumentsBuildTheFullConfinement(t *testing.T) {
	launcher := &Launcher{bwrap: "/usr/bin/bwrap", prlimit: "/usr/bin/prlimit"}
	spec := Spec{
		Argv:     []string{"/bin/cat", "file"},
		Dir:      "/work",
		ReadOnly: []string{"/instructions"},
		Writable: []string{"/home/worker"},
		Env:      map[string]string{"B": "2", "A": "1"},
	}
	args, err := launcher.arguments(spec)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(args, " ")
	if args[0] != "/usr/bin/bwrap" || !strings.Contains(line, "-- /usr/bin/prlimit --cpu=3600") {
		t.Fatalf("limits are not applied inside the sandbox: %s", line)
	}
	for _, want := range []string{
		"--cpu=3600", "--as=4294967296", "--nproc=512",
		"--die-with-parent", "--unshare-user", "--unshare-pid", "--unshare-net",
		"--proc /proc", "--dev /dev", "--tmpfs /tmp",
		"--ro-bind /instructions /instructions",
		"--bind /work /work", "--bind /home/worker /home/worker",
		"--chdir /work", "--clearenv",
		"--setenv A 1 --setenv B 2",
		"-- /bin/cat file",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("arguments miss %q: %s", want, line)
		}
	}
	if spec.Network {
		t.Fatal("unexpected default")
	}
	networked := spec
	networked.Network = true
	args, err = launcher.arguments(networked)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "--unshare-net") {
		t.Fatal("network spec still unshares the network")
	}

	if _, err := launcher.arguments(Spec{Dir: "/work"}); err == nil {
		t.Fatal("empty command accepted")
	}
	if _, err := launcher.arguments(Spec{Argv: []string{"/bin/true"}}); err == nil {
		t.Fatal("missing workdir accepted")
	}
}

func TestConfinedGroupSeesItsOwnPIDNamespace(t *testing.T) {
	launcher := sandbox(t)
	output, err := runIn(t, launcher, Spec{Argv: []string{"/bin/sh", "-c", "echo $$"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if output != "1" && output != "2" {
		t.Fatalf("the group does not own its PID namespace: pid %s", output)
	}
}

func TestConfinementBlocksSystemWritesAndHidesTheHost(t *testing.T) {
	launcher := sandbox(t)
	if output, err := runIn(t, launcher, Spec{Argv: []string{"/bin/sh", "-c", "touch /usr/forbidden"}, Dir: t.TempDir()}); err == nil {
		t.Fatalf("the system is writable: %s", output)
	}
	secret := t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := runIn(t, launcher, Spec{Argv: []string{"/bin/sh", "-c", "ls " + secret + " 2>&1; true"}, Dir: t.TempDir()})
	if err != nil || strings.Contains(output, "secret") {
		t.Fatalf("an unbound host path is visible: %v: %s", err, output)
	}
}

func TestWorkdirIsWritableAndSharedWithTheHost(t *testing.T) {
	launcher := sandbox(t)
	work := t.TempDir()
	if output, err := runIn(t, launcher, Spec{Argv: []string{"/bin/sh", "-c", "echo done > produced.txt"}, Dir: work}); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	produced, err := os.ReadFile(filepath.Join(work, "produced.txt"))
	if err != nil || string(produced) != "done\n" {
		t.Fatalf("produced=%q err=%v", produced, err)
	}
}

func TestEnvironmentIsAnAllowList(t *testing.T) {
	launcher := sandbox(t)
	t.Setenv("MAESTRO_TEST_SECRET", "leaked")
	output, err := runIn(t, launcher, Spec{
		Argv: []string{"/bin/sh", "-c", "echo \"${MAESTRO_TEST_SECRET:-unset} ${GRANTED:-missing}\""},
		Dir:  t.TempDir(),
		Env:  map[string]string{"GRANTED": "yes"},
	})
	if err != nil || output != "unset yes" {
		t.Fatalf("environment leaked: %v: %q", err, output)
	}
}

func TestNetworkIsIsolatedByDefault(t *testing.T) {
	launcher := sandbox(t)
	output, err := runIn(t, launcher, Spec{Argv: []string{"/bin/sh", "-c", "ls /sys/class/net 2>/dev/null; true"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if output != "" && output != "lo" {
		t.Fatalf("host interfaces are visible: %q", output)
	}
}

func TestLimitsAreInheritedInsideTheSandbox(t *testing.T) {
	launcher := sandbox(t)
	output, err := runIn(t, launcher, Spec{
		Argv:   []string{"/bin/sh", "-c", "awk '/Max cpu time|Max address space/{print $4}' /proc/self/limits"},
		Dir:    t.TempDir(),
		Limits: Limits{CPUSeconds: 60, MemoryBytes: 1 << 30, Processes: 64},
	})
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if output != fmt.Sprintf("%d\n%d", 60, 1<<30) {
		t.Fatalf("limits not applied: %q", output)
	}
}

func TestStopKillsEveryDescendant(t *testing.T) {
	launcher := sandbox(t)
	marker := fmt.Sprintf("maestro-launcher-%d", time.Now().UnixNano())
	group, err := launcher.Start(Spec{
		// Shell wrappers carry the marker as their $0, so host-side
		// pgrep can see the descendants of this group and only them.
		Argv: []string{"/bin/sh", "-c",
			"/bin/sh -c 'sleep 1000' " + marker + "-bg & exec /bin/sh -c 'sleep 1001' " + marker + "-fg"},
		Dir: t.TempDir(),
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Let the background descendant exist, then stop the group.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("pgrep", "-f", marker).Run() == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := group.Stop(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// The PID namespace is gone: no descendant may survive.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("pgrep", "-f", marker).Run() != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a descendant survived the stop")
}

func TestStopIsIdempotentAfterExit(t *testing.T) {
	launcher := sandbox(t)
	group, err := launcher.Start(Spec{Argv: []string{"/bin/true"}, Dir: t.TempDir()}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := group.Stop(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestCommandExposesTheConfinedBuilder(t *testing.T) {
	launcher := &Launcher{bwrap: "/usr/bin/bwrap", prlimit: "/usr/bin/prlimit"}
	command, err := launcher.Command(Spec{Argv: []string{"/bin/true"}, Dir: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Path != "/usr/bin/bwrap" || len(command.Env) != 0 {
		t.Fatalf("path=%s env=%v", command.Path, command.Env)
	}
}

func TestReadOnlyWorkdirRefusesWrites(t *testing.T) {
	launcher := sandbox(t)
	work := t.TempDir()
	output, err := runIn(t, launcher, Spec{
		Argv:        []string{"/bin/sh", "-c", "cat /dev/null > attempt.txt"},
		Dir:         work,
		DirReadOnly: true,
	})
	if err == nil {
		t.Fatalf("a read-only workdir accepted a write: %s", output)
	}
	if _, statErr := os.Stat(filepath.Join(work, "attempt.txt")); !os.IsNotExist(statErr) {
		t.Fatal("the write reached the host")
	}
	// The sources stay readable.
	if err := os.WriteFile(filepath.Join(work, "source.txt"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = runIn(t, launcher, Spec{Argv: []string{"/bin/cat", "source.txt"}, Dir: work, DirReadOnly: true})
	if err != nil || output != "kept" {
		t.Fatalf("read failed: %v: %q", err, output)
	}
}
