package main

import (
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
