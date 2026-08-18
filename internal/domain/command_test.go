package domain

import "testing"

func commandInt64(value int64) *int64 { return &value }

func TestCommandEventInputValidateRejectsIllegalOutcomePairings(t *testing.T) {
	tests := []struct {
		name  string
		input CommandEventInput
	}{
		{"exited without exit", CommandEventInput{Key: "go test", KeySource: CommandKeyProgramSubcommand, Program: "go", Status: CommandExited, WallMS: commandInt64(1)}},
		{"signalled with exit", CommandEventInput{Key: "go test", KeySource: CommandKeyProgramSubcommand, Program: "go", Status: CommandSignalled, ExitCode: commandInt64(9), Signal: "KILL", WallMS: commandInt64(1)}},
		{"exited with signal", CommandEventInput{Key: "go test", KeySource: CommandKeyProgramSubcommand, Program: "go", Status: CommandExited, ExitCode: commandInt64(0), Signal: "KILL", WallMS: commandInt64(1)}},
		{"launch failed with wall", CommandEventInput{Key: "missing", KeySource: CommandKeyProgram, Program: "missing", Status: CommandLaunchFailed, WallMS: commandInt64(0)}},
		{"timeout without signal", CommandEventInput{Key: "sleep", KeySource: CommandKeyProgram, Program: "sleep", Status: CommandTimeout, WallMS: commandInt64(1000)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.input.Validate(); err == nil {
				t.Fatalf("Validate accepted illegal pairing: %#v", test.input)
			}
		})
	}
}

func TestCommandEventInputValidateAcceptsEveryLegalOutcome(t *testing.T) {
	tests := []CommandEventInput{
		{Key: "true", KeySource: CommandKeyProgram, Program: "true", Status: CommandExited, ExitCode: commandInt64(0), WallMS: commandInt64(0)},
		{Key: "sleep", KeySource: CommandKeyProgram, Program: "sleep", Status: CommandSignalled, Signal: "TERM", WallMS: commandInt64(4)},
		{Key: "sleep", KeySource: CommandKeyProgram, Program: "sleep", Status: CommandTimeout, Signal: "KILL", WallMS: commandInt64(1000)},
		{Key: "missing", KeySource: CommandKeyProgram, Program: "missing", Status: CommandLaunchFailed},
		{Key: "odd", KeySource: CommandKeyProgram, Program: "odd", Status: CommandUnknown},
		{Key: "odd", KeySource: CommandKeyProgram, Program: "odd", Status: CommandUnknown, WallMS: commandInt64(2)},
	}
	for _, input := range tests {
		if err := input.Validate(); err != nil {
			t.Fatalf("Validate rejected legal pairing %#v: %v", input, err)
		}
	}
}
