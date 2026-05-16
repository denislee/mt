package main

import (
	"fmt"
	"image"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/io/clipboard"
	"gioui.org/io/key"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

type partRow struct {
	p          Partition
	targetEnum widget.Enum
	actionBtn  widget.Clickable
}

type UI struct {
	theme   *material.Theme
	window  *app.Window
	list    widget.List
	refresh widget.Clickable
	copyLog widget.Clickable

	// mu guards rows, status, err. Read on every frame from the UI goroutine
	// and written by doScan/handleAction workers, so prefer the RLock path in
	// layout() to keep render free of contention with background scans.
	mu     sync.RWMutex
	rows   []*partRow
	status string
	err    string

	// targets is replaced wholesale by doScan; readers can grab the current
	// snapshot lock-free.
	targets atomic.Pointer[[]string]

	// busy is set while a scan is in flight; doubles as a re-entry guard so
	// hotplug signals don't pile up extra scans.
	busy atomic.Bool

	// Password prompt state — touched from both UI and worker goroutines.
	pwMu        sync.Mutex
	pwEditor    widget.Editor
	pwSubmit    widget.Clickable
	pwCancel    widget.Clickable
	pwPending   *pwRequest // non-nil while the modal is shown
	cachedPw    string
	lastAuthErr string
	pwFocused   bool
}

func newUI() *UI {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	th.Palette = material.Palette{
		Bg:         rgb(0x14, 0x17, 0x1f),
		Fg:         rgb(0xe6, 0xe9, 0xef),
		ContrastBg: rgb(0x4f, 0x8c, 0xff),
		ContrastFg: rgb(0xff, 0xff, 0xff),
	}
	u := &UI{theme: th}
	u.list.Axis = layout.Vertical
	u.pwEditor.SingleLine = true
	u.pwEditor.Submit = true
	u.pwEditor.Mask = '•'
	empty := []string{}
	u.targets.Store(&empty)
	return u
}

func (u *UI) invalidate() {
	if u.window != nil {
		u.window.Invalidate()
	}
}

func (u *UI) currentTargets() []string {
	if p := u.targets.Load(); p != nil {
		return *p
	}
	return nil
}

func (u *UI) doScan() {
	if !u.busy.CompareAndSwap(false, true) {
		return
	}
	defer u.busy.Store(false)
	u.invalidate()

	parts, scanErr := scan()
	targets := mountTargets()
	u.targets.Store(&targets)

	u.mu.Lock()
	if scanErr != nil {
		u.err = scanErr.Error()
		u.mu.Unlock()
		u.invalidate()
		return
	}
	u.err = ""

	prev := make(map[string]string, len(u.rows))
	for _, r := range u.rows {
		prev[r.p.Path] = r.targetEnum.Value
	}
	u.rows = u.rows[:0]
	for _, p := range parts {
		row := &partRow{p: p}
		if v, ok := prev[p.Path]; ok {
			row.targetEnum.Value = v
		} else if len(targets) > 0 {
			row.targetEnum.Value = guessTarget(p, targets)
		}
		u.rows = append(u.rows, row)
	}
	u.status = fmt.Sprintf("Scanned %s • %d external partition(s)", time.Now().Format("15:04:05"), len(u.rows))
	u.mu.Unlock()
	u.invalidate()
}

func guessTarget(p Partition, targets []string) string {
	lbl := strings.ToLower(p.Label)
	for _, t := range targets {
		base := strings.ToLower(filepath.Base(t))
		if lbl != "" && (base == lbl || strings.Contains(lbl, base) || strings.Contains(base, lbl)) {
			return t
		}
	}
	if p.Tran == "usb" {
		for _, t := range targets {
			if strings.ToLower(filepath.Base(t)) == "usb" {
				return t
			}
		}
	}
	return targets[0]
}

func (u *UI) layout(gtx layout.Context) layout.Dimensions {
	paint.Fill(gtx.Ops, u.theme.Palette.Bg)

	u.pwMu.Lock()
	modal := u.pwPending
	u.pwMu.Unlock()

	// Global "q" / Esc to quit. Suppress while the password modal is open so
	// typing the letter q in the password field doesn't close the app.
	if modal == nil {
		for {
			ev, ok := gtx.Event(
				key.Filter{Name: "Q"},
				key.Filter{Name: key.NameEscape},
			)
			if !ok {
				break
			}
			if ke, ok := ev.(key.Event); ok && ke.State == key.Press {
				logf("key: %q pressed, quitting", ke.Name)
				u.window.Perform(system.ActionClose)
			}
		}
	}

	// Suppress underlying button events while the password modal is open so
	// clicks on the dimmed background don't trigger another mount/unmount.
	if modal == nil && u.refresh.Clicked(gtx) {
		go u.doScan()
	}
	if modal == nil && u.copyLog.Clicked(gtx) {
		u.copyLogToClipboard(gtx)
	}

	u.mu.RLock()
	rows := u.rows
	status := u.status
	errMsg := u.err
	u.mu.RUnlock()
	busy := u.busy.Load()
	targets := u.currentTargets()

	if modal == nil {
		for _, r := range rows {
			if r.actionBtn.Clicked(gtx) {
				go u.handleAction(r)
			}
		}
	}

	dims := layout.Inset{Top: unit.Dp(18), Bottom: unit.Dp(18), Left: unit.Dp(24), Right: unit.Dp(24)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.header(gtx, busy) }),
			layout.Rigid(spacerY(8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.subheader(gtx, status, errMsg) }),
			layout.Rigid(spacerY(16)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if len(rows) == 0 {
					return u.empty(gtx, busy, errMsg)
				}
				return material.List(u.theme, &u.list).Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
					return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return u.card(gtx, rows[i], targets)
					})
				})
			}),
			layout.Rigid(spacerY(10)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(u.theme, "Log: "+logPath)
				lbl.Color = colorMuted
				return lbl.Layout(gtx)
			}),
		)
	})

	if modal != nil {
		u.drawPasswordModal(gtx, modal)
	}
	return dims
}

func (u *UI) header(gtx layout.Context, busy bool) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			t := material.H4(u.theme, "External Storage")
			t.Color = u.theme.Palette.Fg
			return t.Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(u.theme, &u.copyLog, "Copy log")
			btn.Background = colorBadge
			btn.Color = colorBadgeText
			btn.CornerRadius = unit.Dp(8)
			btn.Inset = layout.UniformInset(unit.Dp(10))
			return btn.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "Rescan"
			if busy {
				label = "Scanning…"
			}
			btn := material.Button(u.theme, &u.refresh, label)
			btn.Background = colorAccent
			btn.Color = u.theme.Palette.ContrastFg
			btn.CornerRadius = unit.Dp(8)
			btn.Inset = layout.UniformInset(unit.Dp(10))
			return btn.Layout(gtx)
		}),
	)
}

// copyLogToClipboard reads the current log file and pushes its contents to the
// clipboard via Gio. Updates status with a confirmation or error.
func (u *UI) copyLogToClipboard(gtx layout.Context) {
	content, err := readLog()
	if err != nil {
		logf("copyLog: failed to read log: %v", err)
		u.mu.Lock()
		u.err = "Could not read log: " + err.Error()
		u.mu.Unlock()
		return
	}
	gtx.Execute(clipboard.WriteCmd{
		Type: "application/text",
		Data: io.NopCloser(strings.NewReader(string(content))),
	})
	logf("copyLog: %d bytes copied to clipboard", len(content))
	u.mu.Lock()
	u.status = fmt.Sprintf("Copied log (%d bytes) to clipboard — %s", len(content), logPath)
	u.err = ""
	u.mu.Unlock()
}

func (u *UI) subheader(gtx layout.Context, status, errMsg string) layout.Dimensions {
	txt := status
	col := colorMuted
	if errMsg != "" {
		txt = "Error: " + errMsg
		col = colorWarn
	}
	if txt == "" {
		txt = "Mount USB / SD / removable drives into " + mntRoot
	}
	lbl := material.Body2(u.theme, txt)
	lbl.Color = col
	return lbl.Layout(gtx)
}

func (u *UI) empty(gtx layout.Context, busy bool, errMsg string) layout.Dimensions {
	msg := "No external drives detected.\nPlug one in and press Rescan."
	if busy {
		msg = "Scanning block devices…"
	}
	if errMsg != "" {
		msg = errMsg
	}
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Body1(u.theme, msg)
		lbl.Color = colorMuted
		lbl.Alignment = text.Middle
		return lbl.Layout(gtx)
	})
}

func (u *UI) card(gtx layout.Context, r *partRow, targets []string) layout.Dimensions {
	radius := gtx.Dp(unit.Dp(12))
	macro := op.Record(gtx.Ops)
	dims := layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return u.cardBody(gtx, r, targets)
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

func (u *UI) cardBody(gtx layout.Context, r *partRow, targets []string) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					t := material.Body1(u.theme, r.p.Title())
					t.Color = u.theme.Palette.Fg
					return t.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions { return badge(gtx, u.theme, r.p.Size) }),
				layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					fs := r.p.FSType
					if fs == "" {
						fs = "unknown"
					}
					return badge(gtx, u.theme, fs)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions { return statusPill(gtx, u.theme, r.p.Mounted()) }),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			sub := fmt.Sprintf("%s   •   %s", r.p.Path, r.p.ParentDesc)
			lbl := material.Body2(u.theme, sub)
			lbl.Color = colorMuted
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if r.p.Mounted() {
				lbl := material.Body2(u.theme, "Mounted at "+r.p.Mountpoint)
				lbl.Color = colorOk
				return lbl.Layout(gtx)
			}
			return u.targetPicker(gtx, r, targets)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "Mount"
			bg := colorAccent
			if r.p.Mounted() {
				label = "Unmount"
				bg = colorWarn
			}
			btn := material.Button(u.theme, &r.actionBtn, label)
			btn.Background = bg
			btn.Color = rgb(0xff, 0xff, 0xff)
			btn.CornerRadius = unit.Dp(8)
			btn.Inset = layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(18), Right: unit.Dp(18)}
			return btn.Layout(gtx)
		}),
	)
}

func (u *UI) targetPicker(gtx layout.Context, r *partRow, targets []string) layout.Dimensions {
	if len(targets) == 0 {
		lbl := material.Body2(u.theme, "No directories under "+mntRoot+". Create one to mount here.")
		lbl.Color = colorWarn
		return lbl.Layout(gtx)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(u.theme, "Mount target")
			lbl.Color = colorMuted
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return wrapH(gtx, len(targets), func(gtx layout.Context, i int) layout.Dimensions {
				t := targets[i]
				return layout.Inset{Right: unit.Dp(8), Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return radioChip(gtx, u.theme, &r.targetEnum, t, filepath.Base(t))
				})
			})
		}),
	)
}

func (u *UI) handleAction(r *partRow) {
	logf("action: dev=%s mounted=%v target=%q fstype=%q", r.p.Path, r.p.Mounted(), r.targetEnum.Value, r.p.FSType)
	var out string
	var err error
	if r.p.Mounted() {
		out, err = u.umount(r.p.Path)
	} else {
		target := r.targetEnum.Value
		if target == "" {
			logf("action: no mount target chosen")
			u.mu.Lock()
			u.err = "Pick a mount target under " + mntRoot
			u.mu.Unlock()
			u.invalidate()
			return
		}
		out, err = u.mountAt(r.p.Path, target, r.p.FSType)
	}
	u.mu.Lock()
	if err == errAuthCancelled {
		u.mu.Unlock()
		return
	}
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		u.err = msg
	} else {
		u.err = ""
		if r.p.Mounted() {
			u.status = fmt.Sprintf("Unmounted %s", r.p.Path)
		} else {
			u.status = fmt.Sprintf("Mounted %s on %s (%s)", r.p.Path, r.targetEnum.Value, r.p.FSType)
		}
	}
	u.mu.Unlock()
	u.doScan()
}
