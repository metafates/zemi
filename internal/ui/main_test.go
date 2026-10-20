package ui

import (
	"flag"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	time.Local = time.UTC // a turn's end time in a golden reads the same everywhere
	// parallel boots load the machine, and a single widget render has to
	// stay inside the 1s HardCeiling however loaded the machine is
	flag.Set("test.parallel", "4")
	m.Run()
}
