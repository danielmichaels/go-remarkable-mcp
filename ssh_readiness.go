package main

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func (r *sshReliability) configureReadiness() error {
	for _, s := range []struct {
		name          string
		def, min, max float64
		dst           *time.Duration
	}{
		{"TIMEOUT", 60, .1, 300, &r.readyTimeout}, {"POLL_INTERVAL", 1, .01, 30, &r.poll}, {"SETTLE", 3, 0, 30, &r.settle},
	} {
		value := s.def
		if raw, ok := os.LookupEnv("REMARKABLE_RESTART_" + s.name); ok {
			var err error
			value, err = strconv.ParseFloat(raw, 64)
			if err != nil {
				return fmt.Errorf("invalid REMARKABLE_RESTART_%s", s.name)
			}
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < s.min || value > s.max {
			return fmt.Errorf("REMARKABLE_RESTART_%s must be between %g and %g", s.name, s.min, s.max)
		}
		*s.dst = time.Duration(value * float64(time.Second))
	}
	return nil
}

// ssh -G resolves aliases using the same OpenSSH configuration as operations;
// it does not open a connection or use the password.
func (b *SSHBackend) effectiveHost(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-G", "-p", strconv.Itoa(b.Port), b.User+"@"+b.Host)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("SSH host resolution failed: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "hostname" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("SSH configuration returned no hostname")
}

func sshWebOpen(ctx context.Context, host string) bool {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "80"))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func sshWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
