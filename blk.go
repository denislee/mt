package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const mntRoot = "/mnt"

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
