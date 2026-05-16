package main

import (
	"image"
	"image/color"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

func rgb(r, g, b uint8) color.NRGBA { return color.NRGBA{R: r, G: g, B: b, A: 0xff} }

var (
	colorCard      = rgb(0x1d, 0x21, 0x2d)
	colorCardEdge  = rgb(0x2a, 0x30, 0x40)
	colorMuted     = rgb(0x8a, 0x90, 0xa1)
	colorAccent    = rgb(0x4f, 0x8c, 0xff)
	colorOk        = rgb(0x4a, 0xc7, 0x8c)
	colorWarn      = rgb(0xe0, 0x8a, 0x4a)
	colorBadge     = rgb(0x2a, 0x30, 0x40)
	colorBadgeText = rgb(0xc8, 0xce, 0xdb)
)

func spacerY(dp int) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Spacer{Height: unit.Dp(float32(dp))}.Layout(gtx)
	}
}

// wrapH lays out `count` widgets in a flowing horizontal row, wrapping to
// the next line when the running width would exceed gtx.Constraints.Max.X.
// Avoids the per-frame closure slice that the older []layout.Widget API
// allocated.
func wrapH(gtx layout.Context, count int, child func(gtx layout.Context, i int) layout.Dimensions) layout.Dimensions {
	maxW := gtx.Constraints.Max.X
	gtx2 := gtx
	gtx2.Constraints.Min = image.Point{}

	type placed struct {
		call op.CallOp
		x, y int
	}
	items := make([]placed, 0, count)
	var lineW, lineH, totalH, totalW int
	for i := range count {
		macro := op.Record(gtx.Ops)
		dims := child(gtx2, i)
		call := macro.Stop()
		w, h := dims.Size.X, dims.Size.Y
		if lineW+w > maxW && lineW > 0 {
			totalH += lineH
			lineW = 0
			lineH = 0
		}
		items = append(items, placed{call: call, x: lineW, y: totalH})
		lineW += w
		if h > lineH {
			lineH = h
		}
		if lineW > totalW {
			totalW = lineW
		}
	}
	totalH += lineH
	for _, it := range items {
		tr := op.Offset(image.Point{X: it.x, Y: it.y}).Push(gtx.Ops)
		it.call.Add(gtx.Ops)
		tr.Pop()
	}
	return layout.Dimensions{Size: image.Point{X: totalW, Y: totalH}}
}

func radioChip(gtx layout.Context, th *material.Theme, e *widget.Enum, value, label string) layout.Dimensions {
	radius := gtx.Dp(unit.Dp(14))
	selected := e.Value == value

	return e.Layout(gtx, value, func(gtx layout.Context) layout.Dimensions {
		macro := op.Record(gtx.Ops)
		dims := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, label)
			if selected {
				lbl.Color = rgb(0xff, 0xff, 0xff)
			} else {
				lbl.Color = colorBadgeText
			}
			return lbl.Layout(gtx)
		})
		call := macro.Stop()

		rect := image.Rectangle{Max: dims.Size}
		rr := clip.UniformRRect(rect, radius)
		stack := rr.Push(gtx.Ops)
		bg := colorBadge
		if selected {
			bg = colorAccent
		}
		paint.Fill(gtx.Ops, bg)
		call.Add(gtx.Ops)
		stack.Pop()
		return dims
	})
}

func badge(gtx layout.Context, th *material.Theme, txt string) layout.Dimensions {
	if txt == "" {
		return layout.Dimensions{}
	}
	radius := gtx.Dp(unit.Dp(6))
	macro := op.Record(gtx.Ops)
	dims := layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2), Left: unit.Dp(8), Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Caption(th, txt)
		lbl.Color = colorBadgeText
		return lbl.Layout(gtx)
	})
	call := macro.Stop()
	rect := image.Rectangle{Max: dims.Size}
	rr := clip.UniformRRect(rect, radius)
	stack := rr.Push(gtx.Ops)
	paint.Fill(gtx.Ops, colorBadge)
	call.Add(gtx.Ops)
	stack.Pop()
	return dims
}

func statusPill(gtx layout.Context, th *material.Theme, mounted bool) layout.Dimensions {
	txt := "Not mounted"
	bg := rgb(0x35, 0x2a, 0x1c)
	fg := colorWarn
	if mounted {
		txt = "Mounted"
		bg = rgb(0x12, 0x2e, 0x22)
		fg = colorOk
	}
	radius := gtx.Dp(unit.Dp(10))
	macro := op.Record(gtx.Ops)
	dims := layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(3), Left: unit.Dp(10), Right: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Caption(th, txt)
		lbl.Color = fg
		return lbl.Layout(gtx)
	})
	call := macro.Stop()
	rect := image.Rectangle{Max: dims.Size}
	rr := clip.UniformRRect(rect, radius)
	stack := rr.Push(gtx.Ops)
	paint.Fill(gtx.Ops, bg)
	call.Add(gtx.Ops)
	stack.Pop()
	return dims
}
