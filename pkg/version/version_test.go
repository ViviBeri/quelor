package version

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	if !strings.HasPrefix(String(), "quelor ") || !strings.Contains(String(), Version) {
		t.Fatal(String())
	}
}
