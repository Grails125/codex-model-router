package main

import (
	"reflect"
	"testing"
)

func TestDeepSeekCLIUsesAnExplicitDeepSeekModel(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{"new chat", nil, []string{"--model", "deepseek-flash"}},
		{"resume", []string{"--model", "deepseek-v4-pro", "--resume", "saved-chat"}, []string{"resume", "--model", "deepseek-v4-pro", "--", "saved-chat"}},
		{"reject other models", []string{"--model", "gpt-6.2-sol"}, nil},
		{"reject extra arguments", []string{"--", "--oss"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			args, err := deepSeekCLIArgs(test.args)
			if test.want == nil {
				if err == nil {
					t.Fatal("expected invalid arguments to be rejected")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(args, test.want) {
				t.Fatalf("got args %q, error %v, want %q", args, err, test.want)
			}
		})
	}
}
