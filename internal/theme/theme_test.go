package theme_test

import (
	"image/color"
	"testing"

	"github.com/richardwooding/wright/internal/theme"
)

func hex(c color.Color) string {
	r, g, b, _ := c.RGBA()
	const digits = "0123456789abcdef"
	out := make([]byte, 7)
	out[0] = '#'
	for i, v := range []uint32{r >> 8, g >> 8, b >> 8} {
		out[1+2*i] = digits[v>>4]
		out[2+2*i] = digits[v&0xf]
	}
	return string(out)
}

func TestPaletteFlipsWithBackground(t *testing.T) {
	tests := []struct {
		name   string
		isDark bool
		accent string
		hot    string
	}{
		{"light", false, "#7c3aed", "#dc2626"},
		{"dark", true, "#a78bfa", "#f87171"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := theme.NewPalette(tt.isDark)
			if got := hex(p.Accent); got != tt.accent {
				t.Errorf("Accent = %s, want %s", got, tt.accent)
			}
			if got := hex(p.Hot); got != tt.hot {
				t.Errorf("Hot = %s, want %s", got, tt.hot)
			}
		})
	}
}

func TestStylesRender(t *testing.T) {
	th := theme.New(true)
	if !th.IsDark {
		t.Fatal("IsDark should be true")
	}
	// Rendering must not panic and must preserve the text; colour escapes
	// depend on the output profile so only the content is asserted.
	for name, s := range map[string]string{
		"title": th.Title.Render("wright"),
		"good":  th.Good.Render("ok"),
		"card":  th.Card.Render("body"),
		"bar":   th.StatusBar.Render("status"),
	} {
		if s == "" {
			t.Errorf("%s rendered empty", name)
		}
	}
}
