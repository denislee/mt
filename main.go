package main

import (
	"log"
	"os"

	"gioui.org/app"
	"gioui.org/op"
	"gioui.org/unit"
)

func main() {
	initLog()
	defer func() {
		if logFile != nil {
			_ = logFile.Close()
		}
	}()
	ui := newUI()

	go func() {
		w := new(app.Window)
		w.Option(
			app.Title("mt — external storage"),
			app.Size(unit.Dp(740), unit.Dp(580)),
			app.MinSize(unit.Dp(520), unit.Dp(420)),
		)
		ui.window = w
		go ui.doScan()
		ui.startHotplug()
		if err := run(w, ui); err != nil {
			logf("run: fatal: %v", err)
			log.Fatal(err)
		}
		logf("---- mt exit ----")
		os.Exit(0)
	}()
	app.Main()
}

func run(w *app.Window, ui *UI) error {
	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			ui.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}
