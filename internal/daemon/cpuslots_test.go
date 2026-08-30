//go:build linux

package daemon

import (
	"os"
	"strings"
	"testing"
)

func TestCPUReserveFromEnvAndFloor(t *testing.T) {
	tests := []struct {
		name, value string
		set         bool
		cpus, want  int
		wantErr     bool
	}{
		{name: "default", cpus: 8, want: 7},
		{name: "zero", set: true, value: "0", cpus: 8, want: 8},
		{name: "reserve", set: true, value: "3", cpus: 8, want: 5},
		{name: "floor", set: true, value: "99", cpus: 8, want: 1},
		{name: "malformed", set: true, value: "many", cpus: 8, wantErr: true},
		{name: "negative", set: true, value: "-1", cpus: 8, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.set {
				t.Setenv("AIRA_DAEMON_CPU_RESERVE", test.value)
			} else {
				_ = os.Unsetenv("AIRA_DAEMON_CPU_RESERVE")
			}
			got, err := desiredCPUSlots(test.cpus)
			if test.wantErr {
				if err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
					t.Fatalf("slots=%d err=%v", got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("slots=%d want=%d err=%v", got, test.want, err)
			}
		})
	}
}
