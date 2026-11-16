package main

import (
	"flag"
	"reflect"
	"testing"
)

func TestKernelArgv(t *testing.T) {
	tests := []struct {
		name    string
		kernel  string
		rest    []string
		want    []string
		wantErr bool
	}{
		{"explicit argv wins", "/k/agy_acp_server.par", []string{"/x/kernel", "--flag"}, []string{"/x/kernel", "--flag"}, false},
		{"par gets uid flag", "/k/agy_acp_server.par", nil, []string{"/k/agy_acp_server.par", "--uid="}, false},
		{"plain binary", " /k/agy-acp-server ", nil, []string{"/k/agy-acp-server"}, false},
		{"nothing configured", "", nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := kernelArgv(tt.kernel, tt.rest)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveDisabledTools(t *testing.T) {
	parse := func(args ...string) (*flag.FlagSet, listFlag) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		var l listFlag
		fs.Var(&l, "disabled-tools", "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return fs, l
	}
	fs, l := parse()
	if got := resolveDisabledTools(fs, l); !reflect.DeepEqual(got, defaultDisabledTools) {
		t.Errorf("default: %v", got)
	}
	t.Setenv("AGY_ACP_MUZZLE_DISABLED_TOOLS", "")
	if got := resolveDisabledTools(fs, l); len(got) != 0 {
		t.Errorf("env set empty: %v", got)
	}
	t.Setenv("AGY_ACP_MUZZLE_DISABLED_TOOLS", "schedule, run_command")
	if got := resolveDisabledTools(fs, l); !reflect.DeepEqual(got, listFlag{"schedule", "run_command"}) {
		t.Errorf("env: %v", got)
	}
	fs, l = parse("--disabled-tools", "generate_image")
	if got := resolveDisabledTools(fs, l); !reflect.DeepEqual(got, listFlag{"generate_image"}) {
		t.Errorf("flag over env: %v", got)
	}
}
