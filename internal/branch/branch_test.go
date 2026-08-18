package branch

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{
		"main",
		"agent-a",
		"A.b_c-9",
		strings.Repeat("a", maxNameLength),
	}
	for _, name := range valid {
		t.Run("valid_"+name, func(t *testing.T) {
			if err := ValidateName(name); err != nil {
				t.Fatalf("ValidateName(%q): %v", name, err)
			}
		})
	}

	invalid := []string{
		"",
		".",
		"..",
		"../escape",
		"nested/branch",
		" leading",
		"trailing ",
		"branch:name",
		"café",
		strings.Repeat("a", maxNameLength+1),
	}
	for _, name := range invalid {
		t.Run("invalid_"+name, func(t *testing.T) {
			if err := ValidateName(name); err == nil {
				t.Fatalf("ValidateName(%q) unexpectedly succeeded", name)
			}
		})
	}
}
