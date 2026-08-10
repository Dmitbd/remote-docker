package desktop

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

type productTheme struct {
	fyne.Theme
}

func newProductTheme() fyne.Theme {
	return &productTheme{Theme: theme.DefaultTheme()}
}

func (t *productTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNamePrimary:
		return color.NRGBA{R: 93, G: 83, B: 255, A: 255}
	case theme.ColorNameSuccess:
		return color.NRGBA{R: 43, G: 180, B: 115, A: 255}
	case theme.ColorNameError:
		return color.NRGBA{R: 222, G: 77, B: 92, A: 255}
	default:
		return t.Theme.Color(name, variant)
	}
}
