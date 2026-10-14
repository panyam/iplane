package cmd

import (
	"reflect"
	"strings"
	"testing"
)

// The book prints --engine-args with a space-separated value, and the old
// CSV parse handed vLLM "--max-model-len 32768 --tensor-parallel-size 8" as
// one argument, which it rejects. Both spellings have to work, and so does
// every comma form already in the examples.
func TestSplitEngineArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"space separated", []string{"--max-model-len 32768 --tensor-parallel-size 8"},
			[]string{"--max-model-len", "32768", "--tensor-parallel-size", "8"}},
		{"repeated with equals", []string{"--quantization=fp8", "--max-num-seqs=16"},
			[]string{"--quantization=fp8", "--max-num-seqs=16"}},
		{"comma form", []string{"--quantization,awq_marlin,--max-model-len,16384"},
			[]string{"--quantization", "awq_marlin", "--max-model-len", "16384"}},
		{"comma and space mixed", []string{"--max-model-len=8192, --max-num-seqs=4"},
			[]string{"--max-model-len=8192", "--max-num-seqs=4"}},
		{"tabs and runs of spaces", []string{"  --a\t1   --b 2 "}, []string{"--a", "1", "--b", "2"}},
		{"single quotes keep commas and spaces", []string{`--hf-overrides '{"a": 1, "b": 2}'`},
			[]string{"--hf-overrides", `{"a": 1, "b": 2}`}},
		{"double quotes", []string{`--served-model-name "my model"`},
			[]string{"--served-model-name", "my model"}},
		{"quote inside a word", []string{`--x=a'b c'd`}, []string{"--x=ab cd"}},
		{"backslash escapes a space", []string{`--name a\ b`}, []string{"--name", "a b"}},
		{"empty quoted argument survives", []string{`--chat-template ""`}, []string{"--chat-template", ""}},
		{"empty pieces dropped", []string{"--a,,--b", ""}, []string{"--a", "--b"}},
		{"nil", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := splitEngineArgs(c.in)
			if err != nil {
				t.Fatalf("splitEngineArgs(%q): %v", c.in, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("splitEngineArgs(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSplitEngineArgsRejectsAnUnclosedQuote(t *testing.T) {
	_, err := splitEngineArgs([]string{`--served-model-name "oops`})
	if err == nil {
		t.Fatal("want an error for an unterminated quote")
	}
	if !strings.Contains(err.Error(), "--engine-args") {
		t.Errorf("error = %v, want it to name the flag", err)
	}
}

// The flag itself must not CSV-parse any more: pflag's StringSlice errors on
// a bare double quote, which made a quoted argument impossible to pass.
func TestEngineArgsFlagDoesNotCSVParse(t *testing.T) {
	f := deploymentDeployCmd.Flags().Lookup("engine-args")
	if f == nil {
		t.Fatal("no --engine-args flag")
	}
	if got := f.Value.Type(); got != "stringArray" {
		t.Errorf("--engine-args type = %s, want stringArray (the splitting is ours, not pflag's)", got)
	}
	if !strings.Contains(f.Usage, "space") || !strings.Contains(f.Usage, "comma") {
		t.Errorf("usage %q should state the splitting rule", f.Usage)
	}
}
