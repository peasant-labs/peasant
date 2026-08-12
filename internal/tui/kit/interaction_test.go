package kit_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

func darkTheme() theme.Theme { return theme.New(theme.ModeDark) }

// --- Focusable ---

func TestFocusable_FocusBlurRoundTrip(t *testing.T) {
	th := darkTheme()
	c := kit.NewConfirm(th, "ok?")
	if c.Focused() {
		t.Fatal("new confirm should start blurred")
	}
	c.Focus()
	if !c.Focused() {
		t.Fatal("confirm should be focused after Focus()")
	}
	c.Blur()
	if c.Focused() {
		t.Fatal("confirm should be blurred after Blur()")
	}
}

// --- Confirm: typed result ---

func TestConfirm_EnterOnYesEmitsOK(t *testing.T) {
	c := kit.NewConfirm(darkTheme(), "delete?")
	c.SetSize(30, 3)
	c, _ = c.Update(keyPress(t, "left")) // move highlight to yes
	c, cmd := c.Update(keyPress(t, "enter"))
	msg := runCmd(cmd)
	res, ok := msg.(kit.ConfirmResultMsg)
	if !ok {
		t.Fatalf("expected ConfirmResultMsg, got %T", msg)
	}
	if !res.OK {
		t.Fatal("expected OK=true after selecting yes and confirming")
	}
}

func TestConfirm_EnterDefaultsToNo(t *testing.T) {
	c := kit.NewConfirm(darkTheme(), "delete?")
	c.SetSize(30, 3)
	_, cmd := c.Update(keyPress(t, "enter"))
	res, ok := runCmd(cmd).(kit.ConfirmResultMsg)
	if !ok {
		t.Fatal("expected ConfirmResultMsg")
	}
	if res.OK {
		t.Fatal("a fresh confirm must default to No so a stray Enter cancels")
	}
}

func TestConfirm_BackEmitsCancel(t *testing.T) {
	c := kit.NewConfirm(darkTheme(), "delete?")
	c.SetSize(30, 3)
	c, _ = c.Update(keyPress(t, "left")) // even highlighted on yes...
	_, cmd := c.Update(keyPress(t, "esc"))
	res, ok := runCmd(cmd).(kit.ConfirmResultMsg)
	if !ok {
		t.Fatal("expected ConfirmResultMsg")
	}
	if res.OK {
		t.Fatal("esc/back must cancel regardless of highlight")
	}
}

// --- Overlay: push / pop / typed result navigation ---

func TestOverlay_PushPop(t *testing.T) {
	th := darkTheme()
	o := kit.NewOverlay(th)
	if o.Len() != 0 {
		t.Fatal("new overlay should be empty")
	}
	c := kit.NewConfirm(th, "ok?")
	o = o.Push(c)
	if o.Len() != 1 {
		t.Fatalf("len=%d after push, want 1", o.Len())
	}
	if _, ok := o.Top(); !ok {
		t.Fatal("Top should report the pushed layer")
	}
	o = o.Pop()
	if o.Len() != 0 {
		t.Fatalf("len=%d after pop, want 0", o.Len())
	}
}

func TestOverlay_PopEmptyIsNoOp(t *testing.T) {
	o := kit.NewOverlay(darkTheme())
	o = o.Pop()
	if o.Len() != 0 {
		t.Fatal("popping an empty overlay must be a no-op, not a panic")
	}
}

func TestOverlay_NavigateByTypedResult(t *testing.T) {
	// The parent drives navigation: the layer emits ConfirmResultMsg, the
	// parent pops on it. This exercises the "children emit typed results,
	// parent navigates the stack, no parent pointers" contract.
	th := darkTheme()
	c := kit.NewConfirm(th, "discard?")
	c.SetSize(24, 3)
	o := kit.NewOverlay(th).Push(c)
	o.SetSize(40, 8)

	top, ok := o.Top()
	if !ok {
		t.Fatal("expected a top layer")
	}
	confirm := top.(kit.Confirm)
	_, cmd := confirm.Update(keyPress(t, "enter"))
	if _, ok := runCmd(cmd).(kit.ConfirmResultMsg); !ok {
		t.Fatal("layer should emit a typed ConfirmResultMsg")
	}
	// Parent reacts by popping.
	o = o.Pop()
	if o.Len() != 0 {
		t.Fatal("parent should pop the layer on its typed result")
	}
	if v := o.View("base"); v != "base" {
		t.Fatalf("empty overlay should show base unchanged, got %q", v)
	}
}

// --- Toggle ---

func TestToggle_SpaceFlips(t *testing.T) {
	tg := kit.NewToggle(darkTheme(), "redact", false)
	tg, _ = tg.Update(keyPress(t, "space"))
	if !tg.On() {
		t.Fatal("space should flip toggle on")
	}
	tg, _ = tg.Update(keyPress(t, "space"))
	if tg.On() {
		t.Fatal("space should flip toggle back off")
	}
}

// --- Radio ---

func TestRadio_MoveAndSelect(t *testing.T) {
	r := kit.NewRadio(darkTheme(), []string{"dark", "light", "system"})
	if r.Selected() != 0 {
		t.Fatalf("initial selection %d, want 0", r.Selected())
	}
	r, _ = r.Update(keyPress(t, "down"))
	r, _ = r.Update(keyPress(t, "down"))
	if r.Cursor() != 2 {
		t.Fatalf("cursor=%d after two downs, want 2", r.Cursor())
	}
	r, _ = r.Update(keyPress(t, "space"))
	if r.Selected() != 2 {
		t.Fatalf("selected=%d after select, want 2", r.Selected())
	}
	v, ok := r.SelectedValue()
	if !ok || v != "system" {
		t.Fatalf("selected value %q ok=%v, want system", v, ok)
	}
}

// --- MultiSelect ---

func TestMultiSelect_ToggleAndSelectAll(t *testing.T) {
	m := kit.NewMultiSelect(darkTheme(), []string{"secrets", "paths", "pii"})
	m, _ = m.Update(keyPress(t, "space")) // check index 0
	m, _ = m.Update(keyPress(t, "down"))
	m, _ = m.Update(keyPress(t, "space")) // check index 1
	got := m.Selected()
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("selected=%v, want [0 1]", got)
	}
	// select-all when some are unchecked selects everything.
	m, _ = m.Update(keyPress(t, "a"))
	if len(m.Selected()) != 3 {
		t.Fatalf("select-all should check all 3, got %v", m.Selected())
	}
	// select-all again (all checked) clears everything.
	m, _ = m.Update(keyPress(t, "a"))
	if len(m.Selected()) != 0 {
		t.Fatalf("second select-all should clear, got %v", m.Selected())
	}
}

// --- List: windowed navigation keeps the cursor visible ---

func TestList_WindowedNavigation(t *testing.T) {
	items := []kit.ListItem{
		kit.StringItem("a"), kit.StringItem("b"), kit.StringItem("c"),
		kit.StringItem("d"), kit.StringItem("e"),
	}
	l := kit.NewList(darkTheme(), items)
	l.SetSize(20, 2) // window shows 2 rows
	for i := 0; i < 4; i++ {
		l, _ = l.Update(keyPress(t, "down"))
	}
	if l.Cursor() != 4 {
		t.Fatalf("cursor=%d after 4 downs, want 4", l.Cursor())
	}
	sel, ok := l.Selected()
	if !ok || sel.FilterValue() != "e" {
		t.Fatalf("selected %v ok=%v, want e", sel, ok)
	}
	// Moving down past the end clamps, never panics or overruns.
	l, _ = l.Update(keyPress(t, "down"))
	if l.Cursor() != 4 {
		t.Fatalf("cursor should clamp at last index, got %d", l.Cursor())
	}
}

// --- TextField ---

func TestTextField_Value(t *testing.T) {
	f := kit.NewTextField(darkTheme(), "name")
	f.SetValue("peasant")
	if f.Value() != "peasant" {
		t.Fatalf("value=%q, want peasant", f.Value())
	}
	f.Focus()
	if !f.Focused() {
		t.Fatal("textfield should be focused after Focus()")
	}
}

// --- Spinner: a tick advances the frame ---

func TestSpinner_FrameAdvances(t *testing.T) {
	s := kit.NewSpinner(darkTheme(), "working")
	before := s.CurrentFrame()
	// A zero-value spinner.TickMsg passes the wrapped model's id/tag guards
	// (both are zero) and advances one frame.
	s, _ = s.Update(spinnerTick())
	after := s.CurrentFrame()
	if before == after {
		t.Fatalf("spinner frame did not advance: %q -> %q", before, after)
	}
}

func TestSpinner_LabelRenders(t *testing.T) {
	s := kit.NewSpinner(darkTheme(), "scanning")
	s.SetSize(40, 1)
	if got := s.Label(); got != "scanning" {
		t.Fatalf("label=%q, want scanning", got)
	}
	s = s.SetLabel("publishing")
	if got := s.Label(); got != "publishing" {
		t.Fatalf("label=%q after SetLabel, want publishing", got)
	}
}
