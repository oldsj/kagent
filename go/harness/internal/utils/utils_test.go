package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBoundedBufferConsumesWritesAndBoundsRetainedOutput(t *testing.T) {
	buffer := NewBoundedBuffer(5)
	for _, value := range []string{"abc", "def", "ghi"} {
		written, err := buffer.Write([]byte(value))
		if err != nil || written != len(value) {
			t.Fatalf("Write(%q) = %d, %v", value, written, err)
		}
	}
	if got := buffer.String(); got != "abcde" {
		t.Fatalf("String() = %q, want %q", got, "abcde")
	}
	if got := buffer.Diagnostic(); got != "upstream error (details withheld: stderr truncated)" {
		t.Fatalf("Diagnostic() = %q, want withheld truncated stderr", got)
	}
}

func TestBoundedBufferDiagnostic(t *testing.T) {
	for _, test := range []struct {
		name   string
		limit  int
		writes []string
		want   string
	}{
		{name: "empty", limit: 5, writes: []string{""}, want: ""},
		{name: "exact limit", limit: 5, writes: []string{"abc", "de", ""}, want: "abcde"},
		{name: "overflow in one write", limit: 5, writes: []string{"abcdef"}, want: "upstream error (details withheld: stderr truncated)"},
		{name: "overflow after exact limit", limit: 5, writes: []string{"abcde", "f"}, want: "upstream error (details withheld: stderr truncated)"},
		{name: "zero limit empty", limit: 0, writes: []string{""}, want: ""},
		{name: "zero limit overflow", limit: 0, writes: []string{"a"}, want: "upstream error (details withheld: stderr truncated)"},
		{name: "negative limit overflow", limit: -1, writes: []string{"a"}, want: "upstream error (details withheld: stderr truncated)"},
		{name: "complete capture vetted", limit: 100, writes: []string{"Authori\nzation: Q7m9v2R8d4"}, want: "upstream error (details withheld: possible credential)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			buffer := NewBoundedBuffer(test.limit)
			for _, value := range test.writes {
				written, err := buffer.Write([]byte(value))
				if err != nil || written != len(value) {
					t.Fatalf("Write() = %d, %v, want %d, nil", written, err, len(value))
				}
			}
			if got := buffer.Diagnostic(); got != test.want {
				t.Fatalf("Diagnostic() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReplacePrivateFileAtomicallyReplacesPrivateContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	for _, contents := range []string{"before", "after"} {
		if err := ReplacePrivateFile(path, []byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "after" {
		t.Fatalf("contents = %q, want %q", contents, "after")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file permissions = %v, %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory permissions = %v, %v", info, err)
	}
}
