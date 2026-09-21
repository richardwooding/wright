package toolview_test

import "github.com/charmbracelet/x/ansi"

func strip(s string) string { return ansi.Strip(s) }
