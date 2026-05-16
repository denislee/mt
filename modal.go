package main

import (
	"fmt"
	"image"
	"image/color"
	"strings"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

type pwRequest struct {
	prompt string
	reply  chan pwReply
}

type pwReply struct {
	pw string
	ok bool
}

// askPassword blocks the calling goroutine until the user submits or cancels
// the in-app password dialog. Returns the typed password and ok=false on cancel.
// If a password is already cached from a previous successful command, it is
// returned without prompting.
func (u *UI) askPassword(args []string) (string, bool) {
	u.pwMu.Lock()
	if u.cachedPw != "" {
		pw := u.cachedPw
		u.pwMu.Unlock()
		logf("askPassword: using cached password")
		return pw, true
	}
	logf("askPassword: prompting user for: %s", strings.Join(args, " "))
	req := &pwRequest{
		prompt: humanizeCmd(args),
		reply:  make(chan pwReply, 1),
	}
	u.pwPending = req
	u.pwFocused = false
	u.pwMu.Unlock()
	u.invalidate()

	rep := <-req.reply

	u.pwMu.Lock()
	u.pwPending = nil
	if rep.ok {
		u.cachedPw = rep.pw
		u.lastAuthErr = ""
	}
	u.pwEditor.SetText("")
	u.pwMu.Unlock()
	u.invalidate()
	return rep.pw, rep.ok
}

func humanizeCmd(args []string) string {
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "mount":
		if len(args) >= 2 {
			dev := args[len(args)-2]
			target := args[len(args)-1]
			return fmt.Sprintf("Mount %s at %s", dev, target)
		}
	case "umount":
		if len(args) >= 2 {
			return fmt.Sprintf("Unmount %s", args[1])
		}
	case "chown":
		if len(args) >= 3 {
			return fmt.Sprintf("chown %s", args[2])
		}
	}
	return strings.Join(args, " ")
}

// drawPasswordModal overlays a dimming backdrop and a centered password card.
// It also drives the modal's event handling (Enter, button clicks).
func (u *UI) drawPasswordModal(gtx layout.Context, req *pwRequest) {
	full := image.Rectangle{Max: gtx.Constraints.Max}
	bd := clip.Rect(full).Push(gtx.Ops)
	paint.Fill(gtx.Ops, color.NRGBA{R: 0, G: 0, B: 0, A: 0xb4})
	bd.Pop()

	submit := false
	for {
		ev, ok := u.pwEditor.Update(gtx)
		if !ok {
			break
		}
		if _, isSubmit := ev.(widget.SubmitEvent); isSubmit {
			submit = true
		}
	}
	if u.pwSubmit.Clicked(gtx) {
		submit = true
	}
	if u.pwCancel.Clicked(gtx) {
		u.replyPassword(req, "", false)
		return
	}
	if submit {
		u.replyPassword(req, u.pwEditor.Text(), true)
		return
	}

	if !u.pwFocused {
		gtx.Execute(key.FocusCmd{Tag: &u.pwEditor})
		u.pwFocused = true
	}

	u.pwMu.Lock()
	authErr := u.lastAuthErr
	u.pwMu.Unlock()

	layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		w := min(gtx.Dp(unit.Dp(440)), gtx.Constraints.Max.X-gtx.Dp(unit.Dp(24)))
		gtx.Constraints.Min.X = w
		gtx.Constraints.Max.X = w
		return u.passwordCard(gtx, req, authErr)
	})
}

func (u *UI) replyPassword(req *pwRequest, pw string, ok bool) {
	select {
	case req.reply <- pwReply{pw: pw, ok: ok}:
	default:
	}
}

func (u *UI) passwordCard(gtx layout.Context, req *pwRequest, authErr string) layout.Dimensions {
	radius := gtx.Dp(unit.Dp(14))
	macro := op.Record(gtx.Ops)
	dims := layout.UniformInset(unit.Dp(22)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				t := material.H6(u.theme, "Authentication required")
				t.Color = u.theme.Palette.Fg
				return t.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(u.theme, req.prompt)
				lbl.Color = colorMuted
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.passwordField(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if authErr == "" {
					return layout.Spacer{Height: unit.Dp(18)}.Layout(gtx)
				}
				return layout.Inset{Top: unit.Dp(10), Bottom: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(u.theme, authErr)
					lbl.Color = colorWarn
					return lbl.Layout(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(u.theme, &u.pwCancel, "Cancel")
						btn.Background = colorBadge
						btn.Color = colorBadgeText
						btn.CornerRadius = unit.Dp(8)
						btn.Inset = layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(16), Right: unit.Dp(16)}
						return btn.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(u.theme, &u.pwSubmit, "Authenticate")
						btn.Background = colorAccent
						btn.Color = rgb(0xff, 0xff, 0xff)
						btn.CornerRadius = unit.Dp(8)
						btn.Inset = layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(18), Right: unit.Dp(18)}
						return btn.Layout(gtx)
					}),
				)
			}),
		)
	})
	call := macro.Stop()

	rect := image.Rectangle{Max: dims.Size}
	rr := clip.UniformRRect(rect, radius)
	stack := rr.Push(gtx.Ops)
	paint.Fill(gtx.Ops, colorCard)
	stack.Pop()

	border := clip.Stroke{Path: clip.UniformRRect(rect, radius).Path(gtx.Ops), Width: float32(gtx.Dp(unit.Dp(1)))}.Op()
	paint.FillShape(gtx.Ops, colorCardEdge, border)

	call.Add(gtx.Ops)
	return dims
}

func (u *UI) passwordField(gtx layout.Context) layout.Dimensions {
	radius := gtx.Dp(unit.Dp(8))
	macro := op.Record(gtx.Ops)
	dims := layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		ed := material.Editor(u.theme, &u.pwEditor, "Password")
		ed.Color = u.theme.Palette.Fg
		ed.HintColor = colorMuted
		return ed.Layout(gtx)
	})
	call := macro.Stop()

	rect := image.Rectangle{Max: dims.Size}
	rr := clip.UniformRRect(rect, radius)
	stack := rr.Push(gtx.Ops)
	paint.Fill(gtx.Ops, rgb(0x12, 0x15, 0x1c))
	stack.Pop()

	border := clip.Stroke{Path: clip.UniformRRect(rect, radius).Path(gtx.Ops), Width: float32(gtx.Dp(unit.Dp(1)))}.Op()
	paint.FillShape(gtx.Ops, colorCardEdge, border)

	call.Add(gtx.Ops)
	return dims
}
