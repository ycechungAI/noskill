//go:build !darwin || cgo

// Command noskill-tray is the menu-bar / system-tray app (FR-I7, ADR-0004). It holds no state and
// no logic: internal/tray asks the daemon and builds the menu, this file draws it with
// fyne.io/systray. It is the only binary built with CGO (Cocoa on macOS); a crash here leaves the
// daemon, the CLI and the notifications working.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"

	"noskill/internal/platform"
	"noskill/internal/tray"
)

const (
	approvalSlots = 6
	jobSlots      = 10
	poll          = 3 * time.Second
)

type approvalSlot struct {
	parent, approve, change, skip *systray.MenuItem
	id                            string
}

type jobSlot struct {
	parent, run, toggle *systray.MenuItem
	id                  string
	enabled             bool
}

func main() {
	client, err := tray.Connect(os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, "noskill-tray:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	systray.Run(func() { ready(ctx, client) }, stop)
}

func icon(s tray.State) []byte {
	if runtime.GOOS == "windows" {
		return tray.ICO(s)
	}
	return tray.Icon(s)
}

func ready(ctx context.Context, c *tray.Client) {
	systray.SetIcon(icon(tray.Offline))
	systray.SetTitle("") // an icon only; a title would take menu-bar space on macOS
	systray.SetTooltip("noskill")

	header := systray.AddMenuItem("noskill", "")
	header.Disable()
	problem := systray.AddMenuItem("", "")
	problem.Disable()
	problem.Hide()
	systray.AddSeparator()

	approvals := systray.AddMenuItem("Approvals", "Things waiting for your answer")
	var aslots []*approvalSlot
	none := approvals.AddSubMenuItem("Nothing is waiting", "")
	none.Disable()
	for i := 0; i < approvalSlots; i++ {
		p := approvals.AddSubMenuItem("", "")
		s := &approvalSlot{parent: p, approve: p.AddSubMenuItem("Approve", ""),
			change: p.AddSubMenuItem("Change…", "Change the prefilled values on the status page, then approve there"), skip: p.AddSubMenuItem("Skip", "")}
		p.Hide()
		aslots = append(aslots, s)
	}
	jobs := systray.AddMenuItem("Jobs", "Scheduled jobs")
	noJobs := jobs.AddSubMenuItem("No jobs. Add one with: noskill schedule add", "")
	noJobs.Disable()
	var jslots []*jobSlot
	for i := 0; i < jobSlots; i++ {
		p := jobs.AddSubMenuItem("", "")
		s := &jobSlot{parent: p, run: p.AddSubMenuItem("Run now", ""), toggle: p.AddSubMenuItem("Pause", "")}
		p.Hide()
		jslots = append(jslots, s)
	}
	pauseAll := systray.AddMenuItem("Pause all jobs", "")
	systray.AddSeparator()
	page := systray.AddMenuItem("Open status page", "")
	quit := systray.AddMenuItem("Quit tray (the daemon keeps running)", "")

	var mu sync.Mutex
	var last tray.State = -1
	paused := false
	refresh := func() {
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		st, err := c.Status(cctx)
		m := tray.Build(st, err, time.Now())
		mu.Lock()
		defer mu.Unlock()
		if m.State != last {
			systray.SetIcon(icon(m.State))
			last = m.State
		}
		systray.SetTooltip(m.Tooltip)
		header.SetTitle(m.Header)
		if m.Problem != "" {
			problem.SetTitle(m.Problem)
			problem.Show()
		} else {
			problem.Hide()
		}
		approvals.SetTitle("Approvals")
		if n := len(m.Approvals); n > 0 {
			approvals.SetTitle(fmt.Sprintf("Approvals (%d)", n))
		}
		if len(m.Approvals) == 0 {
			none.Show()
		} else {
			none.Hide()
		}
		for i, s := range aslots {
			if i < len(m.Approvals) {
				e := m.Approvals[i]
				s.id = e.ID
				s.parent.SetTitle(e.Label)
				s.approve.SetTitle(e.Approve)
				if e.Editable {
					s.change.Show()
				} else {
					s.change.Hide()
				}
				s.parent.Show()
			} else {
				s.id = ""
				s.parent.Hide()
			}
		}
		if len(m.Jobs) == 0 {
			noJobs.Show()
		} else {
			noJobs.Hide()
		}
		for i, s := range jslots {
			if i < len(m.Jobs) {
				j := m.Jobs[i]
				s.id, s.enabled = j.ID, j.Enabled
				s.parent.SetTitle(j.Label)
				if j.Enabled {
					s.toggle.SetTitle("Pause")
					s.run.Enable()
				} else {
					s.toggle.SetTitle("Resume")
					s.run.Disable()
				}
				s.parent.Show()
			} else {
				s.id = ""
				s.parent.Hide()
			}
		}
		paused = m.Paused
		if paused {
			pauseAll.SetTitle("Resume all jobs")
		} else {
			pauseAll.SetTitle("Pause all jobs")
		}
	}
	// do runs a daemon call off the menu thread, shows a failure in the menu, and refreshes.
	do := func(f func(context.Context) error) {
		go func() {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := f(cctx); err != nil {
				mu.Lock()
				problem.SetTitle(err.Error())
				problem.Show()
				mu.Unlock()
				return
			}
			refresh()
		}()
	}
	for _, s := range aslots {
		s := s
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-s.approve.ClickedCh:
					mu.Lock()
					id := s.id
					mu.Unlock()
					do(func(c2 context.Context) error { return c.Answer(c2, id, true) })
				case <-s.change.ClickedCh:
					mu.Lock()
					id := s.id
					mu.Unlock()
					go platform.OpenURL(ctx, c.ApprovalURL(id))
				case <-s.skip.ClickedCh:
					mu.Lock()
					id := s.id
					mu.Unlock()
					do(func(c2 context.Context) error { return c.Answer(c2, id, false) })
				}
			}
		}()
	}
	for _, s := range jslots {
		s := s
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-s.run.ClickedCh:
					mu.Lock()
					id := s.id
					mu.Unlock()
					do(func(c2 context.Context) error { return c.Job(c2, id, "run") })
				case <-s.toggle.ClickedCh:
					mu.Lock()
					id, on := s.id, s.enabled
					mu.Unlock()
					act := "pause"
					if !on {
						act = "resume"
					}
					do(func(c2 context.Context) error { return c.Job(c2, id, act) })
				}
			}
		}()
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				systray.Quit()
				return
			case <-pauseAll.ClickedCh:
				mu.Lock()
				act := "pause"
				if paused {
					act = "resume"
				}
				mu.Unlock()
				do(func(c2 context.Context) error { return c.Job(c2, "all", act) })
			case <-page.ClickedCh:
				go platform.OpenURL(ctx, c.PageURL())
			case <-quit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
	go func() {
		refresh()
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
}
