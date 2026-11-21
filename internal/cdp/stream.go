package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultLinger is how long the screencast keeps running after its last viewer leaves, so a
// reloading or reconnecting window does not stop and restart Chromium's encoder.
const DefaultLinger = 5 * time.Second

// firstFrameWait is how long a newly started screencast may take to send its first frame before
// one is captured with Page.captureScreenshot instead. Chromium normally sends one at once, but a
// page that is not repainting can leave a new viewer with nothing to draw.
const firstFrameWait = time.Second

// retryWait is how long the stream worker waits before retrying a failed Page.startScreencast.
const retryWait = time.Second

// screencastParams are the Page.startScreencast settings. They match what the agent always used.
var screencastParams = map[string]any{
	"format":        "jpeg",
	"quality":       90,
	"maxWidth":      2560,
	"maxHeight":     1600,
	"everyNthFrame": 1,
}

// Subscribe registers a live viewer and starts the screencast if it is not running. It returns
// the sequence number to pass to WaitNewer and a release function. While the stream is already
// running the cached frame is current, so since is 0 and the viewer gets it at once; otherwise
// since is the cached frame's number, so the viewer waits for a fresh one instead of drawing a
// stale page. Release is safe to call more than once; the stream stops Linger after the last
// release unless a new viewer arrives first.
func (s *Screencast) Subscribe() (since uint64, release func()) {
	s.mu.Lock()
	s.viewers++
	since = s.baselineLocked()
	s.mu.Unlock()
	s.wake()
	var once sync.Once
	return since, func() {
		once.Do(func() {
			s.mu.Lock()
			s.viewers--
			s.lastDemand = time.Now()
			s.mu.Unlock()
			s.wake()
		})
	}
}

// Keep records demand from a client that polls instead of holding a subscription, such as
// GET /api/browser/frame?since=N. The stream starts if needed and runs for at least Linger after
// the last Keep. It returns the same since value as Subscribe.
func (s *Screencast) Keep() uint64 {
	s.mu.Lock()
	s.lastDemand = time.Now()
	since := s.baselineLocked()
	s.mu.Unlock()
	s.wake()
	return since
}

// Streaming reports whether Page.startScreencast is currently in effect.
func (s *Screencast) Streaming() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streaming
}

// Viewers returns the number of live subscriptions.
func (s *Screencast) Viewers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewers
}

// baselineLocked returns 0 when the cached frame is current, or the cached sequence number when
// a caller must wait for a fresh frame. s.mu must be held.
func (s *Screencast) baselineLocked() uint64 {
	if s.streaming && s.seq > 0 {
		return 0
	}
	return s.seq
}

// wake nudges the stream worker without blocking.
func (s *Screencast) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// streamWorker is the only goroutine that starts or stops the screencast, so the two never race.
// It runs until the connection ends.
func (s *Screencast) streamWorker(life context.Context) {
	defer close(s.workerDone)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-life.Done():
			return
		case <-s.done:
			return
		case <-s.kick:
		case <-timer.C:
		}
		if next := s.reconcile(life); next > 0 {
			timer.Reset(next)
		}
	}
}

// reconcile starts or stops the screencast to match demand. It returns how long to wait before
// checking again, or 0 when only a new kick can change anything.
func (s *Screencast) reconcile(life context.Context) time.Duration {
	s.mu.Lock()
	now := time.Now()
	want := s.viewers > 0
	var remaining time.Duration
	if !want && !s.lastDemand.IsZero() {
		remaining = s.linger - now.Sub(s.lastDemand)
		want = remaining > 0
	}
	streaming := s.streaming
	if !want && streaming {
		// Mark it stopped before the call, so a viewer arriving now waits for a fresh frame and
		// kicks the worker to start again.
		s.streaming = false
	}
	seq := s.seq
	s.mu.Unlock()

	switch {
	case want && !streaming:
		if err := s.callLife(life, "Page.startScreencast", screencastParams); err != nil {
			if errors.Is(err, ErrClosed) || life.Err() != nil {
				return 0
			}
			return retryWait
		}
		s.mu.Lock()
		s.streaming = true
		s.mu.Unlock()
		go s.ensureFirstFrame(life, seq)
	case !want && streaming:
		_ = s.callLife(life, "Page.stopScreencast", nil)
		return 0
	}
	if remaining > 0 && s.Viewers() == 0 {
		return remaining
	}
	return 0
}

// callLife runs one call bounded by the connection's lifetime and callTimeout.
func (s *Screencast) callLife(life context.Context, method string, params any) error {
	ctx, cancel := context.WithTimeout(life, callTimeout)
	defer cancel()
	_, err := s.Call(ctx, method, params)
	return err
}

// ensureFirstFrame captures a frame if the screencast has not sent one newer than since within
// firstFrameWait, so a new viewer always has something to draw.
func (s *Screencast) ensureFirstFrame(life context.Context, since uint64) {
	ctx, cancel := context.WithTimeout(life, firstFrameWait)
	_, _, err := s.WaitNewer(ctx, since)
	cancel()
	if err == nil || life.Err() != nil || errors.Is(err, ErrClosed) {
		return
	}
	cctx, ccancel := context.WithTimeout(life, callTimeout)
	defer ccancel()
	img, err := s.Capture(cctx, "jpeg")
	if err != nil {
		return
	}
	s.mu.Lock()
	stale := s.seq > since
	s.mu.Unlock()
	if !stale {
		s.storeFrame(img)
	}
}

// Capture takes one screenshot of the page with Page.captureScreenshot, independent of the
// screencast. format is "jpeg" (quality 90, the screencast's setting) or "png" (lossless, better
// for reading text); an empty format means jpeg.
func (s *Screencast) Capture(ctx context.Context, format string) ([]byte, error) {
	params := map[string]any{}
	switch format {
	case "", "jpeg":
		params["format"] = "jpeg"
		params["quality"] = 90
	case "png":
		params["format"] = "png"
	default:
		return nil, fmt.Errorf("cdp: unsupported screenshot format %q", format)
	}
	raw, err := s.Call(ctx, "Page.captureScreenshot", params)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	img, err := base64.StdEncoding.DecodeString(r.Data)
	if err != nil {
		return nil, err
	}
	if len(img) == 0 {
		return nil, errors.New("cdp: empty screenshot")
	}
	return img, nil
}
