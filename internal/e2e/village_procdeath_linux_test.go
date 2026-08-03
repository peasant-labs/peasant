//go:build e2e && linux

package e2e

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	procStatStateToken     = 0
	procStatStartTimeToken = 19
	procStatStartTimeField = 22
)

func TestVillageProcDeathKillsGrandchildOnHelperDeath(t *testing.T) {
	if getenv(envVillageProcDeathHelper) == "1" {
		runVillageProcDeathHelper()
		return
	}

	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not found on PATH")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestVillageProcDeathKillsGrandchildOnHelperDeath$")
	cmd.Env = append(os.Environ(), envAssignment(envVillageProcDeathHelper, "1"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start procdeath helper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	})

	sleepPID := readHelperPID(t, stdout)
	sleepStat, err := readProcStatSnapshot(sleepPID)
	if err != nil {
		t.Fatalf("capture sleep grandchild pid %d /proc start time: %v", sleepPID, err)
	}
	t.Cleanup(func() {
		dead, err := processDeadZombieOrReused(sleepPID, sleepStat.startTime)
		if err == nil && !dead {
			_ = syscall.Kill(sleepPID, syscall.SIGKILL)
		}
	})

	// This proves the tracked sleep process is still alive before killing its
	// helper. The start-time index is covered separately by the boot-time
	// proc-stat test below.
	dead, err := processDeadZombieOrReused(sleepPID, sleepStat.startTime)
	if err != nil {
		t.Fatalf("pre-kill inspect sleep pid %d: %v", sleepPID, err)
	}
	if dead {
		t.Fatalf("sleep grandchild pid %d reported dead/reused before helper SIGKILL", sleepPID)
	}

	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL procdeath helper pid %d: %v", cmd.Process.Pid, err)
	}
	_, _ = cmd.Process.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dead, err := processDeadZombieOrReused(sleepPID, sleepStat.startTime)
		if err != nil {
			t.Fatalf("inspect sleep pid %d: %v", sleepPID, err)
		}
		if dead {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sleep grandchild pid %d survived helper SIGKILL; setVillageProcDeath may not be setting Pdeathsig", sleepPID)
}

func TestProcStatStartTimeTokenIndex(t *testing.T) {
	if procStatStartTimeToken != procStatStartTimeField-3 {
		t.Fatalf("starttime token index = %d, want field %d -> token %d",
			procStatStartTimeToken, procStatStartTimeField, procStatStartTimeField-3)
	}

	if _, err := exec.LookPath("getconf"); err != nil {
		t.Skipf("getconf not found on PATH: %v", err)
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not found on PATH: %v", err)
	}

	ticksPerSecond, err := procClockTicksPerSecond()
	if err != nil {
		t.Fatalf("read clock ticks per second: %v", err)
	}
	uptimeBefore, err := readProcUptimeSeconds()
	if err != nil {
		t.Fatalf("read /proc/uptime before child start: %v", err)
	}

	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start proc-stat starttime child: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	})

	childSnapshot, err := readProcStatSnapshot(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("capture child pid %d /proc start time: %v", cmd.Process.Pid, err)
	}
	uptimeAfter, err := readProcUptimeSeconds()
	if err != nil {
		t.Fatalf("read /proc/uptime after child snapshot: %v", err)
	}

	slackTicks := procStatStartTimeSlackTicks(ticksPerSecond)
	lowerBound, _ := procUptimeTickBounds(uptimeBefore, ticksPerSecond, slackTicks)
	_, upperBound := procUptimeTickBounds(uptimeAfter, ticksPerSecond, slackTicks)
	if childSnapshot.startTime < lowerBound || childSnapshot.startTime > upperBound {
		t.Fatalf("child pid %d starttime = %d, want within boot-time tick bounds [%d, %d] from uptime before %.2fs, uptime after %.2fs, CLK_TCK %d, slack %d",
			cmd.Process.Pid, childSnapshot.startTime, lowerBound, upperBound, uptimeBefore, uptimeAfter, ticksPerSecond, slackTicks)
	}
}

func TestProcStatSnapshotDeadZombieOrReused(t *testing.T) {
	const originalStartTime = 42
	tests := []struct {
		name     string
		snapshot procStatSnapshot
		wantDead bool
	}{
		{
			name:     "live same start time",
			snapshot: procStatSnapshot{state: 'S', startTime: originalStartTime},
			wantDead: false,
		},
		{
			name:     "zombie same start time",
			snapshot: procStatSnapshot{state: 'Z', startTime: originalStartTime},
			wantDead: true,
		},
		{
			name:     "live different start time",
			snapshot: procStatSnapshot{state: 'S', startTime: originalStartTime + 1},
			wantDead: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDead := procStatSnapshotDeadZombieOrReused(tt.snapshot, originalStartTime)
			if gotDead != tt.wantDead {
				t.Fatalf("procStatSnapshotDeadZombieOrReused(%+v, %d) = %t, want %t",
					tt.snapshot, originalStartTime, gotDead, tt.wantDead)
			}
		})
	}
}

func runVillageProcDeathHelper() {
	lockVillageLaunchThread()
	cmd := exec.Command("sleep", "60")
	setVillageProcDeath(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start sleep grandchild: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(cmd.Process.Pid)
	select {}
}

func readHelperPID(t *testing.T, stdout io.Reader) int {
	t.Helper()
	type result struct {
		pid int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				ch <- result{err: err}
				return
			}
			ch <- result{err: errors.New("helper exited before reporting sleep pid")}
			return
		}
		line := strings.TrimSpace(scanner.Text())
		pid, err := strconv.Atoi(line)
		if err != nil {
			ch <- result{err: fmt.Errorf("parse helper pid %q: %w", line, err)}
			return
		}
		ch <- result{pid: pid}
	}()

	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("read procdeath helper pid: %v", got.err)
		}
		if got.pid <= 0 {
			t.Fatalf("procdeath helper reported invalid pid %d", got.pid)
		}
		return got.pid
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for procdeath helper to report sleep pid")
		return 0
	}
}

type procStatSnapshot struct {
	state     byte
	startTime uint64
}

func processDeadZombieOrReused(pid int, originalStartTime uint64) (bool, error) {
	snapshot, err := readProcStatSnapshot(pid)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return procStatSnapshotDeadZombieOrReused(snapshot, originalStartTime), nil
}

func procStatSnapshotDeadZombieOrReused(snapshot procStatSnapshot, originalStartTime uint64) bool {
	return snapshot.state == 'Z' || snapshot.startTime != originalStartTime
}

func readProcStatSnapshot(pid int) (procStatSnapshot, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procStatSnapshot{}, err
	}
	return parseProcStatSnapshot(pid, data)
}

func procClockTicksPerSecond() (uint64, error) {
	out, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		return 0, fmt.Errorf("getconf CLK_TCK: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	ticks, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse getconf CLK_TCK %q: %w", raw, err)
	}
	if ticks == 0 {
		return 0, fmt.Errorf("getconf CLK_TCK returned zero")
	}
	return ticks, nil
}

func readProcUptimeSeconds() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("unexpected /proc/uptime format: %q", data)
	}
	uptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/uptime seconds %q: %w", fields[0], err)
	}
	if math.IsNaN(uptime) || math.IsInf(uptime, 0) || uptime < 0 {
		return 0, fmt.Errorf("invalid /proc/uptime seconds %q", fields[0])
	}
	return uptime, nil
}

func procStatStartTimeSlackTicks(ticksPerSecond uint64) uint64 {
	slack := ticksPerSecond / 10
	if slack < 2 {
		return 2
	}
	return slack
}

func procUptimeTickBounds(uptimeSeconds float64, ticksPerSecond, slackTicks uint64) (uint64, uint64) {
	ticks := uptimeSeconds * float64(ticksPerSecond)
	lower := uint64(math.Floor(ticks))
	upper := uint64(math.Ceil(ticks))
	if lower > slackTicks {
		lower -= slackTicks
	} else {
		lower = 0
	}
	return lower, upper + slackTicks
}

func parseProcStatSnapshot(pid int, data []byte) (procStatSnapshot, error) {
	tokens, err := procStatTokens(pid, data)
	if err != nil {
		return procStatSnapshot{}, err
	}
	if len(tokens[procStatStateToken]) != 1 {
		return procStatSnapshot{}, fmt.Errorf("unexpected /proc/%d/stat state token: %q", pid, tokens[procStatStateToken])
	}
	startTime, err := strconv.ParseUint(tokens[procStatStartTimeToken], 10, 64)
	if err != nil {
		return procStatSnapshot{}, fmt.Errorf("parse /proc/%d/stat starttime %q: %w", pid, tokens[procStatStartTimeToken], err)
	}
	return procStatSnapshot{state: tokens[procStatStateToken][0], startTime: startTime}, nil
}

func procStatTokens(pid int, data []byte) ([]string, error) {
	idx := strings.LastIndex(string(data), ") ")
	if idx < 0 || idx+2 >= len(data) {
		return nil, fmt.Errorf("unexpected /proc/%d/stat format: %q", pid, data)
	}
	tokens := strings.Fields(string(data[idx+2:]))
	if len(tokens) <= procStatStartTimeToken {
		return nil, fmt.Errorf("unexpected /proc/%d/stat token count %d: %q", pid, len(tokens), data)
	}
	return tokens, nil
}
