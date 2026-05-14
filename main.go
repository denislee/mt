package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/io/clipboard"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const mntRoot = "/mnt"

// ---------- logging ----------

var (
	logger  *log.Logger
	logPath string
	logFile *os.File
)

// initLog sets up a file-backed logger at $XDG_CACHE_HOME/mt/mt.log (falling
// back to /tmp/mt.log). Output also mirrors to stderr.
func initLog() {
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cache = filepath.Join(home, ".cache")
		}
	}
	dir := filepath.Join(cache, "mt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dir = "/tmp"
	}
	logPath = filepath.Join(dir, "mt.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logPath = "/tmp/mt.log"
		f, _ = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	logFile = f
	var w io.Writer = os.Stderr
	if f != nil {
		w = io.MultiWriter(os.Stderr, f)
	}
	logger = log.New(w, "", log.LstdFlags|log.Lmicroseconds)
	logger.Printf("---- mt starting (pid=%d, go=%s, %s/%s) ----", os.Getpid(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	logger.Printf("log file: %s", logPath)
}

func logf(format string, args ...interface{}) {
	if logger == nil {
		return
	}
	logger.Output(2, fmt.Sprintf(format, args...))
}

// readLog returns the current contents of the log file plus the path.
func readLog() (string, error) {
	if logFile != nil {
		_ = logFile.Sync()
	}
	if logPath == "" {
		return "", fmt.Errorf("log not initialised")
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---------- block device model ----------

type blkDev struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Size       string   `json:"size"`
	Type       string   `json:"type"`
	Mountpoint string   `json:"mountpoint"`
	Label      string   `json:"label"`
	FSType     string   `json:"fstype"`
	Tran       string   `json:"tran"`
	RM         bool     `json:"rm"`
	Hotplug    bool     `json:"hotplug"`
	Vendor     string   `json:"vendor"`
	Model      string   `json:"model"`
	Children   []blkDev `json:"children"`
}

type lsblkOut struct {
	BlockDevices []blkDev `json:"blockdevices"`
}

type Partition struct {
	Path       string
	Size       string
	Label      string
	FSType     string
	Mountpoint string
	ParentDesc string
	Tran       string
}

func (p Partition) Title() string {
	switch {
	case p.Label != "":
		return p.Label
	case p.FSType != "":
		return p.FSType + " on " + filepath.Base(p.Path)
	default:
		return filepath.Base(p.Path)
	}
}

func (p Partition) Mounted() bool { return p.Mountpoint != "" }

func scan() ([]Partition, error) {
	logf("scan: invoking lsblk")
	cmd := exec.Command("lsblk", "-J", "-o",
		"NAME,PATH,SIZE,TYPE,MOUNTPOINT,LABEL,FSTYPE,TRAN,RM,HOTPLUG,VENDOR,MODEL")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		logf("scan: lsblk failed: %v", err)
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var data lsblkOut
	if err := json.Unmarshal(out, &data); err != nil {
		logf("scan: parse lsblk JSON failed: %v", err)
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}

	var parts []Partition
	for _, disk := range data.BlockDevices {
		if !isExternal(disk) {
			continue
		}
		desc := describe(disk)
		if len(disk.Children) == 0 && disk.FSType != "" {
			parts = append(parts, mkPart(disk, desc))
			continue
		}
		for _, c := range disk.Children {
			if c.Type != "part" && c.Type != "crypt" && c.Type != "lvm" {
				continue
			}
			parts = append(parts, mkPart(c, desc))
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Path < parts[j].Path })
	logf("scan: %d external partition(s) detected", len(parts))
	for _, p := range parts {
		logf("scan:   %s size=%s fstype=%q label=%q mount=%q parent=%q",
			p.Path, p.Size, p.FSType, p.Label, p.Mountpoint, p.ParentDesc)
	}
	return parts, nil
}

func mkPart(b blkDev, parent string) Partition {
	return Partition{
		Path:       b.Path,
		Size:       b.Size,
		Label:      b.Label,
		FSType:     b.FSType,
		Mountpoint: b.Mountpoint,
		ParentDesc: parent,
		Tran:       b.Tran,
	}
}

func isExternal(d blkDev) bool {
	if d.Type != "disk" {
		return false
	}
	switch strings.ToLower(d.Tran) {
	case "usb", "mmc", "ieee1394", "sbp":
		return true
	}
	return d.RM
}

func describe(d blkDev) string {
	name := strings.TrimSpace(d.Vendor + " " + d.Model)
	if name == "" {
		name = d.Path
	}
	tran := d.Tran
	if tran == "" {
		tran = "removable"
	}
	return fmt.Sprintf("%s (%s, %s)", name, tran, d.Size)
}

func mountTargets() []string {
	entries, err := os.ReadDir(mntRoot)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(mntRoot, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// errAuthCancelled is returned when the user dismisses the password dialog.
var errAuthCancelled = fmt.Errorf("authentication cancelled")

// runPrivileged runs the given command as root. Already-root → exec directly.
// Otherwise pipes a password (collected via the in-app prompt) to sudo -S.
// The password is cached for the session; on auth failure the cache is cleared.
func (u *UI) runPrivileged(args ...string) (string, error) {
	logf("priv: invoke %s", strings.Join(args, " "))
	if os.Geteuid() == 0 {
		logf("priv: euid=0, executing directly")
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		out, err := cmd.CombinedOutput()
		s := strings.TrimSpace(string(out))
		logf("priv: result err=%v output=%q", err, truncate(s, 400))
		return s, err
	}
	for attempt := 1; ; attempt++ {
		pw, ok := u.askPassword(args)
		if !ok {
			logf("priv: user cancelled password prompt for: %s", strings.Join(args, " "))
			return "", errAuthCancelled
		}
		sudoArgs := append([]string{"-S", "-k", "-p", "", "--"}, args...)
		logf("priv: attempt %d via sudo (cached_pw=%v)", attempt, pw != "" )
		cmd := exec.Command("sudo", sudoArgs...)
		cmd.Stdin = strings.NewReader(pw + "\n")
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		out, err := cmd.CombinedOutput()
		txt := strings.TrimSpace(string(out))
		if err != nil && isAuthError(txt) {
			logf("priv: authentication failed (attempt %d), clearing cache", attempt)
			u.pwMu.Lock()
			u.cachedPw = ""
			u.lastAuthErr = "Incorrect password."
			u.pwMu.Unlock()
			continue
		}
		txt = stripSudoNoise(txt)
		if err != nil {
			logf("priv: command failed err=%v output=%q", err, truncate(txt, 400))
		} else {
			logf("priv: command succeeded output=%q", truncate(txt, 400))
		}
		return txt, err
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func isAuthError(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "incorrect password") ||
		strings.Contains(low, "sorry, try again") ||
		strings.Contains(low, "authentication failure") ||
		strings.Contains(low, "3 incorrect password attempts")
}

func stripSudoNoise(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if trim == "" || strings.HasPrefix(trim, "Password:") || strings.HasPrefix(trim, "Sorry,") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// mountAt builds fstype-appropriate options so the current user has read/write.
// Foreign filesystems (NTFS / FAT / exFAT / ISO / HFS+) honor uid/gid/umask at
// mount time; native UNIX filesystems use real permissions, so we chown the
// mountpoint to the invoking user after the mount succeeds.
func (u *UI) mountAt(dev, target, fstype string) (string, error) {
	uid := os.Getuid()
	gid := os.Getgid()
	if sudoUID := os.Getenv("SUDO_UID"); sudoUID != "" {
		fmt.Sscanf(sudoUID, "%d", &uid)
	}
	if sudoGID := os.Getenv("SUDO_GID"); sudoGID != "" {
		fmt.Sscanf(sudoGID, "%d", &gid)
	}

	var opts []string
	var driverType string // explicit -t value, empty = let mount pick
	needChown := false
	switch strings.ToLower(fstype) {
	case "ntfs", "ntfs3":
		// Force the read/write kernel driver — without -t, mount may pick the
		// legacy read-only "ntfs" driver and reject modern options.
		driverType = "ntfs3"
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022,windows_names", uid, gid)}
	case "vfat", "msdos":
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022,flush", uid, gid)}
	case "exfat":
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022", uid, gid)}
	case "iso9660", "udf":
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022,ro", uid, gid)}
	case "hfsplus", "hfs":
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022,force", uid, gid)}
	default:
		// ext2/3/4, btrfs, xfs, f2fs, etc. — native Unix permissions.
		needChown = true
	}

	buildArgs := func(extraOpt string) []string {
		a := []string{"mount"}
		if driverType != "" {
			a = append(a, "-t", driverType)
		}
		o := append([]string(nil), opts...)
		if extraOpt != "" {
			o = append(o, extraOpt)
		}
		for _, x := range o {
			a = append(a, "-o", x)
		}
		return append(a, dev, target)
	}

	args := buildArgs("")
	logf("mount: dev=%s target=%s fstype=%q driver=%q opts=%v needChown=%v",
		dev, target, fstype, driverType, opts, needChown)

	out, err := u.runPrivileged(args...)

	// ntfs3 writes the actual failure reason to the kernel ring buffer rather
	// than to stderr. Grab the tail so the user has a real diagnostic, then
	// retry once with `force` (which lets ntfs3 mount R/W over a dirty journal
	// / clear hibernation state on the next clean unmount).
	if err != nil && driverType == "ntfs3" {
		u.logDmesgTail("ntfs3")
		logf("mount: NTFS first attempt failed, retrying with -o force")
		args = buildArgs("force")
		out2, err2 := u.runPrivileged(args...)
		if err2 == nil {
			out, err = out2, nil
		} else {
			u.logDmesgTail("ntfs3")
			out = strings.TrimSpace(out + "\n" + out2)
			err = err2
		}
	}

	if err != nil {
		return out, err
	}
	if needChown {
		who := fmt.Sprintf("%d:%d", uid, gid)
		if out2, err2 := u.runPrivileged("chown", who, target); err2 != nil {
			return strings.TrimSpace(out + "\n" + out2), err2
		}
	}
	return out, nil
}

// logDmesgTail fetches the last few kernel-ring lines mentioning the given
// keyword (privileged, since dmesg is root-only by default) and writes them
// into the app log.
func (u *UI) logDmesgTail(keyword string) {
	// `dmesg -T` adds human timestamps; `--ctime` is a synonym on util-linux.
	out, err := u.runPrivileged("sh", "-c",
		fmt.Sprintf("dmesg -T 2>/dev/null | grep -i %q | tail -n 20", keyword))
	if err != nil {
		logf("dmesg: could not read kernel log: %v", err)
		return
	}
	if strings.TrimSpace(out) == "" {
		logf("dmesg: no %q messages found in the kernel ring buffer", keyword)
		return
	}
	logf("dmesg tail (filter=%q):\n%s", keyword, out)
}

func looksLikeDirtyNTFS(msg string) bool {
	low := strings.ToLower(msg)
	for _, hint := range []string{
		"unsafe state",
		"hibern",
		"was not cleanly unmounted",
		"dirty",
		"falling back",
		"read-only",
	} {
		if strings.Contains(low, hint) {
			return true
		}
	}
	return false
}

func (u *UI) umount(dev string) (string, error) {
	logf("umount: dev=%s", dev)
	return u.runPrivileged("umount", dev)
}

// ---------- UI ----------

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

	mu      sync.Mutex
	rows    []*partRow
	targets []string
	status  string
	err     string
	busy    bool

	// Password prompt state — touched from both UI and worker goroutines.
	pwMu        sync.Mutex
	pwEditor    widget.Editor
	pwSubmit    widget.Clickable
	pwCancel    widget.Clickable
	pwPending   *pwRequest // non-nil while the modal is shown
	cachedPw    string
	lastAuthErr string
	pwFocused   bool // tracks whether we've already moved focus this open
}

type pwRequest struct {
	prompt string
	reply  chan pwReply
}

type pwReply struct {
	pw string
	ok bool
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
	return u
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
		// mount [-o opts ...] dev target
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

func (u *UI) invalidate() {
	if u.window != nil {
		u.window.Invalidate()
	}
}

func (u *UI) doScan() {
	u.mu.Lock()
	u.busy = true
	u.mu.Unlock()
	u.invalidate()

	parts, err := scan()
	targets := mountTargets()

	u.mu.Lock()
	u.busy = false
	u.targets = targets
	if err != nil {
		u.err = err.Error()
		u.mu.Unlock()
		u.invalidate()
		return
	}
	u.err = ""

	prev := map[string]string{}
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

	// Suppress underlying button events while the password modal is open so
	// clicks on the dimmed background don't trigger another mount/unmount.
	if modal == nil && u.refresh.Clicked(gtx) {
		go u.doScan()
	}
	if modal == nil && u.copyLog.Clicked(gtx) {
		u.copyLogToClipboard(gtx)
	}

	u.mu.Lock()
	rows := u.rows
	status := u.status
	errMsg := u.err
	busy := u.busy
	targets := append([]string(nil), u.targets...)
	u.mu.Unlock()

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

func spacerY(dp int) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Spacer{Height: unit.Dp(float32(dp))}.Layout(gtx)
	}
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
		Data: io.NopCloser(strings.NewReader(content)),
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
					t := material.H6(u.theme, r.p.Title())
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
			widgets := make([]layout.Widget, 0, len(targets))
			for _, t := range targets {
				t := t
				widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Right: unit.Dp(8), Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return radioChip(gtx, u.theme, &r.targetEnum, t, filepath.Base(t))
					})
				})
			}
			return wrapHorizontal(gtx, widgets)
		}),
	)
}

func wrapHorizontal(gtx layout.Context, widgets []layout.Widget) layout.Dimensions {
	maxW := gtx.Constraints.Max.X
	var lineW, lineH, totalH, totalW int
	type placed struct {
		call op.CallOp
		x, y int
	}
	var items []placed
	for _, w := range widgets {
		macro := op.Record(gtx.Ops)
		gtx2 := gtx
		gtx2.Constraints.Min = image.Point{}
		dims := w(gtx2)
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

// drawPasswordModal overlays a dimming backdrop and a centered password card.
// It also drives the modal's event handling (Enter, button clicks).
func (u *UI) drawPasswordModal(gtx layout.Context, req *pwRequest) {
	// 1) Dim the rest of the window.
	full := image.Rectangle{Max: gtx.Constraints.Max}
	bd := clip.Rect(full).Push(gtx.Ops)
	paint.Fill(gtx.Ops, color.NRGBA{R: 0, G: 0, B: 0, A: 0xb4})
	bd.Pop()

	// 2) Handle editor events (Enter to submit).
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

	// 3) Focus the editor the first frame the modal appears.
	if !u.pwFocused {
		gtx.Execute(key.FocusCmd{Tag: &u.pwEditor})
		u.pwFocused = true
	}

	// 4) Centered card.
	u.pwMu.Lock()
	authErr := u.lastAuthErr
	u.pwMu.Unlock()

	layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		w := gtx.Dp(unit.Dp(440))
		if w > gtx.Constraints.Max.X-gtx.Dp(unit.Dp(24)) {
			w = gtx.Constraints.Max.X - gtx.Dp(unit.Dp(24))
		}
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

// ---------- main ----------

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
