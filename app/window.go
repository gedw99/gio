// SPDX-License-Identifier: Unlicense OR MIT

package app

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"runtime"
	"time"

	"gioui.org/f32"
	"gioui.org/gpu"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/io/profile"
	"gioui.org/io/router"
	"gioui.org/io/system"
	"gioui.org/op"
	"gioui.org/unit"

	_ "gioui.org/app/internal/log"
)

// Option configures a window.
type Option func(unit.Metric, *Config)

// Window represents an operating system window.
type Window struct {
	ctx context
	gpu gpu.GPU

	// driverFuncs is a channel of functions to run when
	// the Window has a valid driver.
	driverFuncs chan func(d driver)
	// wakeups wakes up the native event loop to send a
	// WakeupEvent that flushes driverFuncs.
	wakeups chan struct{}
	// wakeupFuncs is sent wakeup functions when the driver changes.
	wakeupFuncs chan func()
	// redraws is notified when a redraw is requested by the client.
	redraws chan struct{}
	// immediateRedraws is like redraw but doesn't need a wakeup.
	immediateRedraws chan struct{}
	// scheduledRedraws is sent the most recent delayed redraw time.
	scheduledRedraws chan time.Time

	waiting    bool
	waitEvents []event.Event
	defers     []func(d driver)
	out        chan event.Event
	frames     chan *op.Ops
	frameAck   chan struct{}
	// dead is closed when the window is destroyed.
	dead chan struct{}

	stage        system.Stage
	animating    bool
	hasNextFrame bool
	nextFrame    time.Time
	delayedDraw  *time.Timer

	queue  queue
	cursor pointer.CursorName

	callbacks callbacks

	nocontext bool

	// semantic data, lazily evaluated if requested by a backend to speed up
	// the cases where semantic data is not needed.
	semantic struct {
		// uptodate tracks whether the fields below are up to date.
		uptodate bool
		root     router.SemanticID
		prevTree []router.SemanticNode
		tree     []router.SemanticNode
		ids      map[router.SemanticID]router.SemanticNode
	}
}

type callbacks struct {
	w *Window
	d driver
}

// queue is an event.Queue implementation that distributes system events
// to the input handlers declared in the most recent frame.
type queue struct {
	q router.Router
}

// Pre-allocate the ack event to avoid garbage.
var ackEvent event.Event

// NewWindow creates a new window for a set of window
// options. The options are hints; the platform is free to
// ignore or adjust them.
//
// If the current program is running on iOS or Android,
// NewWindow returns the window previously created by the
// platform.
//
// Calling NewWindow more than once is not supported on
// iOS, Android, WebAssembly.
func NewWindow(options ...Option) *Window {
	defaultOptions := []Option{
		Size(unit.Dp(800), unit.Dp(600)),
		Title("Gio"),
	}
	options = append(defaultOptions, options...)
	var cnf Config
	cnf.apply(unit.Metric{}, options)

	w := &Window{
		out:              make(chan event.Event),
		immediateRedraws: make(chan struct{}, 0),
		redraws:          make(chan struct{}, 1),
		scheduledRedraws: make(chan time.Time, 1),
		frames:           make(chan *op.Ops),
		frameAck:         make(chan struct{}),
		driverFuncs:      make(chan func(d driver), 1),
		wakeups:          make(chan struct{}, 1),
		wakeupFuncs:      make(chan func()),
		dead:             make(chan struct{}),
		nocontext:        cnf.CustomRenderer,
	}
	w.semantic.ids = make(map[router.SemanticID]router.SemanticNode)
	w.callbacks.w = w
	go w.run(options)
	return w
}

// Events returns the channel where events are delivered.
func (w *Window) Events() <-chan event.Event {
	return w.out
}

// update the window contents, input operations declare input handlers,
// and so on. The supplied operations list completely replaces the window state
// from previous calls.
func (w *Window) update(frame *op.Ops) {
	w.frames <- frame
	<-w.frameAck
}

func (w *Window) validateAndProcess(d driver, size image.Point, sync bool, frame *op.Ops) error {
	for {
		if w.gpu == nil && !w.nocontext {
			var err error
			if w.ctx == nil {
				w.ctx, err = d.NewContext()
				if err != nil {
					return err
				}
				sync = true
			}
		}
		if sync && w.ctx != nil {
			if err := w.ctx.Refresh(); err != nil {
				if errors.Is(err, errOutOfDate) {
					// Surface couldn't be created for transient reasons. Skip
					// this frame and wait for the next.
					return nil
				}
				w.destroyGPU()
				if errors.Is(err, gpu.ErrDeviceLost) {
					continue
				}
				return err
			}
		}
		if w.gpu == nil && !w.nocontext {
			if err := w.ctx.Lock(); err != nil {
				w.destroyGPU()
				return err
			}
			gpu, err := gpu.New(w.ctx.API())
			w.ctx.Unlock()
			if err != nil {
				w.destroyGPU()
				return err
			}
			w.gpu = gpu
		}
		if w.gpu != nil {
			if err := w.render(frame, size); err != nil {
				if errors.Is(err, errOutOfDate) {
					// GPU surface needs refreshing.
					sync = true
					continue
				}
				w.destroyGPU()
				if errors.Is(err, gpu.ErrDeviceLost) {
					continue
				}
				return err
			}
		}
		w.queue.q.Frame(frame)
		return nil
	}
}

func (w *Window) render(frame *op.Ops, viewport image.Point) error {
	if err := w.ctx.Lock(); err != nil {
		return err
	}
	defer w.ctx.Unlock()
	if runtime.GOOS == "js" {
		// Use transparent black when Gio is embedded, to allow mixing of Gio and
		// foreign content below.
		w.gpu.Clear(color.NRGBA{A: 0x00, R: 0x00, G: 0x00, B: 0x00})
	} else {
		w.gpu.Clear(color.NRGBA{A: 0xff, R: 0xff, G: 0xff, B: 0xff})
	}
	target, err := w.ctx.RenderTarget()
	if err != nil {
		return err
	}
	if err := w.gpu.Frame(frame, target, viewport); err != nil {
		return err
	}
	return w.ctx.Present()
}

func (w *Window) processFrame(d driver, frameStart time.Time) {
	for k := range w.semantic.ids {
		delete(w.semantic.ids, k)
	}
	w.semantic.uptodate = false
	q := &w.queue.q
	switch q.TextInputState() {
	case router.TextInputOpen:
		d.ShowTextInput(true)
	case router.TextInputClose:
		d.ShowTextInput(false)
	}
	if hint, ok := q.TextInputHint(); ok {
		d.SetInputHint(hint)
	}
	if txt, ok := q.WriteClipboard(); ok {
		d.WriteClipboard(txt)
	}
	if q.ReadClipboard() {
		d.ReadClipboard()
	}
	if q.Profiling() && w.gpu != nil {
		frameDur := time.Since(frameStart)
		frameDur = frameDur.Truncate(100 * time.Microsecond)
		quantum := 100 * time.Microsecond
		timings := fmt.Sprintf("tot:%7s %s", frameDur.Round(quantum), w.gpu.Profile())
		q.Queue(profile.Event{Timings: timings})
	}
	if t, ok := q.WakeupTime(); ok {
		w.setNextFrame(t)
	}
	w.updateAnimation(d)
}

// Invalidate the window such that a FrameEvent will be generated immediately.
// If the window is inactive, the event is sent when the window becomes active.
//
// Note that Invalidate is intended for externally triggered updates, such as a
// response from a network request. InvalidateOp is more efficient for animation
// and similar internal updates.
//
// Invalidate is safe for concurrent use.
func (w *Window) Invalidate() {
	select {
	case w.immediateRedraws <- struct{}{}:
		return
	default:
	}
	select {
	case w.redraws <- struct{}{}:
		w.wakeup()
	default:
	}
}

// Option applies the options to the window.
func (w *Window) Option(opts ...Option) {
	w.driverDefer(func(d driver) {
		d.Configure(opts)
	})
}

// ReadClipboard initiates a read of the clipboard in the form
// of a clipboard.Event. Multiple reads may be coalesced
// to a single event.
func (w *Window) ReadClipboard() {
	w.driverDefer(func(d driver) {
		d.ReadClipboard()
	})
}

// WriteClipboard writes a string to the clipboard.
func (w *Window) WriteClipboard(s string) {
	w.driverDefer(func(d driver) {
		d.WriteClipboard(s)
	})
}

// SetCursorName changes the current window cursor to name.
func (w *Window) SetCursorName(name pointer.CursorName) {
	w.driverDefer(func(d driver) {
		d.SetCursor(name)
	})
}

// Close the window. The window's event loop should exit when it receives
// system.DestroyEvent.
//
// Currently, only macOS, Windows, X11 and Wayland drivers implement this functionality,
// all others are stubbed.
func (w *Window) Close() {
	w.driverDefer(func(d driver) {
		d.Close()
	})
}

// Run f in the same thread as the native window event loop, and wait for f to
// return or the window to close. Run is guaranteed not to deadlock if it is
// invoked during the handling of a ViewEvent, system.FrameEvent,
// system.StageEvent; call Run in a separate goroutine to avoid deadlock in all
// other cases.
//
// Note that most programs should not call Run; configuring a Window with
// CustomRenderer is a notable exception.
func (w *Window) Run(f func()) {
	done := make(chan struct{})
	w.driverDefer(func(d driver) {
		defer close(done)
		f()
	})
	select {
	case <-done:
	case <-w.dead:
	}
}

// driverDefer is like Run but can be run from any context. It doesn't wait
// for f to return.
func (w *Window) driverDefer(f func(d driver)) {
	select {
	case w.driverFuncs <- f:
		w.wakeup()
	case <-w.dead:
	}
}

func (w *Window) updateAnimation(d driver) {
	animate := false
	if w.stage >= system.StageRunning && w.hasNextFrame {
		if dt := time.Until(w.nextFrame); dt <= 0 {
			animate = true
		} else {
			// Schedule redraw.
			select {
			case <-w.scheduledRedraws:
			default:
			}
			w.scheduledRedraws <- w.nextFrame
		}
	}
	if animate != w.animating {
		w.animating = animate
		d.SetAnimating(animate)
	}
}

func (w *Window) wakeup() {
	select {
	case w.wakeups <- struct{}{}:
	default:
	}
}

func (w *Window) setNextFrame(at time.Time) {
	if !w.hasNextFrame || at.Before(w.nextFrame) {
		w.hasNextFrame = true
		w.nextFrame = at
	}
}

func (c *callbacks) SetDriver(d driver) {
	c.d = d
	var wakeup func()
	if d != nil {
		wakeup = d.Wakeup
	}
	c.w.wakeupFuncs <- wakeup
}

func (c *callbacks) Event(e event.Event) {
	if c.d == nil {
		panic("event while no driver active")
	}
	if c.w.waiting {
		c.w.waitEvents = append(c.w.waitEvents, e)
		return
	}
	c.w.processEvent(c.d, e)
	for len(c.w.waitEvents) > 0 {
		we := c.w.waitEvents[0]
		copy(c.w.waitEvents, c.w.waitEvents[1:])
		c.w.waitEvents = c.w.waitEvents[:len(c.w.waitEvents)-1]
		c.w.processEvent(c.d, we)
	}
	for _, f := range c.w.defers {
		f(c.d)
	}
	c.w.defers = nil
	c.w.updateState(c.d)
}

// SemanticRoot returns the ID of the semantic root.
func (c *callbacks) SemanticRoot() router.SemanticID {
	c.w.updateSemantics()
	return c.w.semantic.root
}

// LookupSemantic looks up a semantic node from an ID. The zero ID denotes the root.
func (c *callbacks) LookupSemantic(semID router.SemanticID) (router.SemanticNode, bool) {
	c.w.updateSemantics()
	n, found := c.w.semantic.ids[semID]
	return n, found
}

func (c *callbacks) AppendSemanticDiffs(diffs []router.SemanticID) []router.SemanticID {
	c.w.updateSemantics()
	if tree := c.w.semantic.prevTree; len(tree) > 0 {
		c.w.collectSemanticDiffs(&diffs, c.w.semantic.prevTree[0])
	}
	return diffs
}

func (c *callbacks) SemanticAt(pos f32.Point) (router.SemanticID, bool) {
	c.w.updateSemantics()
	return c.w.queue.q.SemanticAt(pos)
}

func (w *Window) waitAck() {
	w.waiting = true
	defer func() {
		w.waiting = false
	}()
	for {
		select {
		case f := <-w.driverFuncs:
			w.defers = append(w.defers, f)
		case w.out <- ackEvent:
			// A dummy event went through, so we know the application has processed the previous event.
			return
		case <-w.immediateRedraws:
			// Invalidate was called during frame processing.
			w.setNextFrame(time.Time{})
		}
	}
}

func (w *Window) destroyGPU() {
	if w.gpu != nil {
		w.ctx.Lock()
		w.gpu.Release()
		w.ctx.Unlock()
		w.gpu = nil
	}
	if w.ctx != nil {
		w.ctx.Release()
		w.ctx = nil
	}
}

// waitFrame waits for the client to either call FrameEvent.Frame
// or to continue event handling. It returns whether the client
// called Frame or not.
func (w *Window) waitFrame() (*op.Ops, bool) {
	w.waiting = true
	defer func() {
		w.waiting = false
	}()
	for {
		select {
		case f := <-w.driverFuncs:
			w.defers = append(w.defers, f)
		case frame := <-w.frames:
			// The client called FrameEvent.Frame.
			return frame, true
		case w.out <- ackEvent:
			// The client ignored FrameEvent and continued processing
			// events.
			return nil, false
		case <-w.immediateRedraws:
			// Invalidate was called during frame processing.
			w.setNextFrame(time.Time{})
		}
	}
}

// updateSemantics refreshes the semantics tree, the id to node map and the ids of
// updated nodes.
func (w *Window) updateSemantics() {
	if w.semantic.uptodate {
		return
	}
	w.semantic.uptodate = true
	w.semantic.prevTree, w.semantic.tree = w.semantic.tree, w.semantic.prevTree
	w.semantic.tree = w.queue.q.AppendSemantics(w.semantic.tree[:0])
	w.semantic.root = w.semantic.tree[0].ID
	for _, n := range w.semantic.tree {
		w.semantic.ids[n.ID] = n
	}
}

// collectSemanticDiffs traverses the previous semantic tree, noting changed nodes.
func (w *Window) collectSemanticDiffs(diffs *[]router.SemanticID, n router.SemanticNode) {
	newNode, exists := w.semantic.ids[n.ID]
	// Ignore deleted nodes, as their disappearance will be reported through an
	// ancestor node.
	if !exists {
		return
	}
	diff := newNode.Desc != n.Desc || len(n.Children) != len(newNode.Children)
	for i, ch := range n.Children {
		if !diff {
			newCh := newNode.Children[i]
			diff = ch.ID != newCh.ID
		}
		w.collectSemanticDiffs(diffs, ch)
	}
	if diff {
		*diffs = append(*diffs, n.ID)
	}
}

func (w *Window) updateState(d driver) {
	for {
		select {
		case <-w.redraws:
			w.setNextFrame(time.Time{})
			w.updateAnimation(d)
		default:
			return
		}
	}
}

func (w *Window) processEvent(d driver, e event.Event) {
	select {
	case <-w.dead:
		return
	default:
	}
	switch e2 := e.(type) {
	case system.StageEvent:
		if e2.Stage < system.StageRunning {
			if w.gpu != nil {
				w.ctx.Lock()
				w.gpu.Release()
				w.gpu = nil
				w.ctx.Unlock()
			}
		}
		w.stage = e2.Stage
		w.updateAnimation(d)
		w.out <- e
		w.waitAck()
	case frameEvent:
		if e2.Size == (image.Point{}) {
			panic(errors.New("internal error: zero-sized Draw"))
		}
		if w.stage < system.StageRunning {
			// No drawing if not visible.
			break
		}
		frameStart := time.Now()
		w.hasNextFrame = false
		e2.Frame = w.update
		e2.Queue = &w.queue
		w.out <- e2.FrameEvent
		frame, gotFrame := w.waitFrame()
		err := w.validateAndProcess(d, e2.Size, e2.Sync, frame)
		if gotFrame {
			// We're done with frame, let the client continue.
			w.frameAck <- struct{}{}
		}
		if err != nil {
			w.destroyGPU()
			w.out <- system.DestroyEvent{Err: err}
			close(w.dead)
			close(w.out)
			break
		}
		w.processFrame(d, frameStart)
		w.updateCursor(d)
	case *system.CommandEvent:
		w.out <- e
		w.waitAck()
	case system.DestroyEvent:
		w.destroyGPU()
		w.out <- e2
		close(w.dead)
		close(w.out)
	case ViewEvent:
		w.out <- e2
		w.waitAck()
	case wakeupEvent:
	case event.Event:
		if w.queue.q.Queue(e2) {
			w.setNextFrame(time.Time{})
			w.updateAnimation(d)
		}
		w.updateCursor(d)
		w.out <- e
	}
}

func (w *Window) run(options []Option) {
	if err := newWindow(&w.callbacks, options); err != nil {
		w.out <- system.DestroyEvent{Err: err}
		close(w.dead)
		close(w.out)
		return
	}
	var wakeup func()
	var timer *time.Timer
	for {
		var (
			wakeups <-chan struct{}
			timeC   <-chan time.Time
		)
		if wakeup != nil {
			wakeups = w.wakeups
			if timer != nil {
				timeC = timer.C
			}
		}
		select {
		case t := <-w.scheduledRedraws:
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(time.Until(t))
		case <-w.dead:
			return
		case <-timeC:
			select {
			case w.redraws <- struct{}{}:
				wakeup()
			default:
			}
		case <-wakeups:
			wakeup()
		case wakeup = <-w.wakeupFuncs:
		}
	}
}

func (w *Window) updateCursor(d driver) {
	if c := w.queue.q.Cursor(); c != w.cursor {
		w.cursor = c
		d.SetCursor(c)
	}
}

// Raise requests that the platform bring this window to the top of all open windows.
// Some platforms do not allow this except under certain circumstances, such as when
// a window from the same application already has focus. If the platform does not
// support it, this method will do nothing.
func (w *Window) Raise() {
	w.driverDefer(func(d driver) {
		d.Raise()
	})
}

func (q *queue) Events(k event.Tag) []event.Event {
	return q.q.Events(k)
}

// Title sets the title of the window.
func Title(t string) Option {
	return func(_ unit.Metric, cnf *Config) {
		cnf.Title = t
	}
}

// Size sets the size of the window. The mode will be changed to Windowed.
func Size(w, h unit.Value) Option {
	if w.V <= 0 {
		panic("width must be larger than or equal to 0")
	}
	if h.V <= 0 {
		panic("height must be larger than or equal to 0")
	}
	return func(m unit.Metric, cnf *Config) {
		cnf.Mode = Windowed
		cnf.Size = image.Point{
			X: m.Px(w),
			Y: m.Px(h),
		}
	}
}

// Center is an option to center the window on the screen.
// The option is ignored in Fullscreen mode.
func Centered() Option {
	return func(m unit.Metric, cnf *Config) {
		// Set the flag so the driver can later do the actual centering.
		cnf.center = true
	}
}

// MaxSize sets the maximum size of the window.
func MaxSize(w, h unit.Value) Option {
	if w.V <= 0 {
		panic("width must be larger than or equal to 0")
	}
	if h.V <= 0 {
		panic("height must be larger than or equal to 0")
	}
	return func(m unit.Metric, cnf *Config) {
		cnf.MaxSize = image.Point{
			X: m.Px(w),
			Y: m.Px(h),
		}
	}
}

// MinSize sets the minimum size of the window.
func MinSize(w, h unit.Value) Option {
	if w.V <= 0 {
		panic("width must be larger than or equal to 0")
	}
	if h.V <= 0 {
		panic("height must be larger than or equal to 0")
	}
	return func(m unit.Metric, cnf *Config) {
		cnf.MinSize = image.Point{
			X: m.Px(w),
			Y: m.Px(h),
		}
	}
}

// StatusColor sets the color of the Android status bar.
func StatusColor(color color.NRGBA) Option {
	return func(_ unit.Metric, cnf *Config) {
		cnf.StatusColor = color
	}
}

// NavigationColor sets the color of the navigation bar on Android, or the address bar in browsers.
func NavigationColor(color color.NRGBA) Option {
	return func(_ unit.Metric, cnf *Config) {
		cnf.NavigationColor = color
	}
}

// CustomRenderer controls whether the window contents is
// rendered by the client. If true, no GPU context is created.
func CustomRenderer(custom bool) Option {
	return func(_ unit.Metric, cnf *Config) {
		cnf.CustomRenderer = custom
	}
}
