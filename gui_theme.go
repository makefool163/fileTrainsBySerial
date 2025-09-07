package main

import (
	"fyne.io/fyne/v2"
)

type customTheme struct {
	fyne.Theme
	scaleFactor float32 // 缩小比例，0.7 表示缩小 30%
}

func NewCustomTheme(scale float32) fyne.Theme {
	return &customTheme{
		Theme:       fyne.CurrentApp().Settings().Theme(),
		scaleFactor: scale,
	}
}

func (t *customTheme) Size(name fyne.ThemeSizeName) float32 {
	base := t.Theme.Size(name)
	return base * t.scaleFactor
}
