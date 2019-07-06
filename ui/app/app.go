// SPDX-License-Identifier: Unlicense OR MIT

package app

import (
	"image"
	"os"
	"strings"

	"gioui.org/ui"
)

type Event interface {
	ImplementsEvent()
}

type DrawEvent struct {
	Config ui.Config
	Size   image.Point
	// Whether this draw is system generated
	// and needs a complete frame before
	// proceeding.
	sync bool
}

type StageEvent struct {
	Stage Stage
}

// CommandEvent is a system event.
type CommandEvent struct {
	Type CommandType
	// Suppress the default action of the command.
	Cancel bool
}

type Stage uint8
type CommandType uint8

type Input interface {
	ImplementsInput()
}

const (
	StageDead Stage = iota
	StagePaused
	StageRunning
)

const (
	// CommandBack is the command for a back action
	// such as the Android back button.
	CommandBack CommandType = iota
)

const (
	inchPrDp = 1.0 / 160
	mmPrDp   = 25.4 / 160
	// monitorScale is the extra scale applied to
	// monitor outputs to compensate for the extra
	// viewing distance compared to phone and tables.
	monitorScale = 1.50
	// minDensity is the minimum pixels per dp to
	// ensure font and ui legibility on low-dpi
	// screens.
	minDensity = 1.25
)

// extraArgs contains extra arguments to append to
// os.Args. The arguments are separated with |.
// Useful for running programs on mobiles where the
// command line is not available.
// Set with the go linker flag -X.
var extraArgs string

var windows = make(chan *Window)

// CreateWindow creates a new window for a set of window
// options. The options are hints; the platform is free to
// ignore or adjust them.
// CreateWindow is not supported on iOS and Android.
func CreateWindow(opts *WindowOptions) error {
	if opts == nil {
		opts = &WindowOptions{
			Width:  ui.Dp(800),
			Height: ui.Dp(600),
			Title:  "Gio program",
		}
	}
	if opts.Width.V <= 0 || opts.Height.V <= 0 {
		panic("window width and height must be larger than 0")
	}
	return createWindow(opts)
}

func Windows() <-chan *Window {
	return windows
}

func (l Stage) String() string {
	switch l {
	case StageDead:
		return "StageDead"
	case StagePaused:
		return "StagePaused"
	case StageRunning:
		return "StageRunning"
	default:
		panic("unexpected Stage value")
	}
}

func (_ DrawEvent) ImplementsEvent()     {}
func (_ StageEvent) ImplementsEvent()    {}
func (_ *CommandEvent) ImplementsEvent() {}

func init() {
	args := strings.Split(extraArgs, "|")
	os.Args = append(os.Args, args...)
}

// DataDir returns a path to use for application-specific
// configuration data.
// On desktop systems, DataDir use os.UserConfigDir.
// On iOS NSDocumentDirectory is queried.
// For Android Context.getFilesDir is used.
func DataDir() (string, error) {
	return dataDir()
}
