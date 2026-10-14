package ui

import (
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	time.Local = time.UTC // a turn's end time in a golden reads the same everywhere
	m.Run()
}
