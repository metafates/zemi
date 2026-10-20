package ui

import (
	"flag"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	time.Local = time.UTC // a turn's end time in a golden reads the same everywhere
	// parallel boots load the machine, and a single widget render has to
	// stay inside the 1s HardCeiling however loaded the machine is; past
	// two the boots cost more than they overlap
	flag.Set("test.parallel", "2")
	m.Run()
}
