package extensions

import (
	"example.test/hookdep"
	"testing"
)

func TestDependency(t *testing.T) {
	if hookdep.Value != 42 {
		t.Fatal("wrong immutable dependency")
	}
}
