package qr

import (
	"errors"
	"fmt"
	"image"
	"math"
	"sort"
)

// DefaultTrackGap is the gap a FrameTracker bridges by default: a symbol
// that fails to read for up to this many frames is reported as one
// continuous track when it reads again.
const DefaultTrackGap = 5

// defaultMaxTracks bounds the symbols a FrameTracker keeps at once, and
// with them its memory, however many the frames hold.
const defaultMaxTracks = 256

// FrameTracker aggregates the symbols decoded from an ordered sequence of
// images, such as the frames of a camera scanning a printed poster. The
// same physical symbol, followed by its content and position, is one
// [SymbolTrack] across the frames even when a few frames cannot read it,
// for example because glare covers it. Since v2.8.
//
// AddFrame decodes a frame with DecodeAll and folds its symbols into the
// tracks; AddResults folds symbols decoded already, which lets a caller
// keep its own decoding loop. Tracks returns the tracks seen so far. A
// FrameTracker is not safe for concurrent use; run one per stream and
// decode frames concurrently in separate trackers, as Decode itself is
// safe for concurrent use.
type FrameTracker struct {
	opts      []DecodeOption
	maxGap    int
	maxTracks int

	frame  int // index of the next frame, from 0
	order  int // creation order of the next track
	size   image.Point
	tracks []*symbolTrack
	err    error // an invalid option, reported on every call
}

// TrackOption configures a [FrameTracker].
type TrackOption func(*FrameTracker)

// WithTrackGap sets how many consecutive frames a symbol may fail to read
// without its track ending: a symbol seen again within gap frames of its
// last appearance continues the same track, and a longer absence ends it.
// gap must be non-negative; zero means every frame must read the symbol.
// The default is [DefaultTrackGap]. Since v2.8.
func WithTrackGap(gap int) TrackOption {
	return func(t *FrameTracker) {
		if gap < 0 {
			t.err = fmt.Errorf("%w: WithTrackGap(%d): want a non-negative gap", ErrInvalidArgument, gap)
			return
		}
		t.maxGap = gap
	}
}

// WithMaxTracks bounds the number of tracks the tracker keeps, and with it
// the memory a long stream with many different symbols can use. When a
// frame holds a new symbol and the limit is reached, the track whose last
// appearance is oldest is retired, so that tracks returned afterwards no
// longer include it. n must be at least 1. The default is 256. Since v2.8.
func WithMaxTracks(n int) TrackOption {
	return func(t *FrameTracker) {
		if n < 1 {
			t.err = fmt.Errorf("%w: WithMaxTracks(%d): want at least 1", ErrInvalidArgument, n)
			return
		}
		t.maxTracks = n
	}
}

// NewFrameTracker returns a [FrameTracker] that decodes each frame with the
// given DecodeOptions. [WithoutInverted] is a typical option for a camera
// stream. Since v2.8.
func NewFrameTracker(opts ...TrackOption) *FrameTracker {
	t := &FrameTracker{maxGap: DefaultTrackGap, maxTracks: defaultMaxTracks}
	for _, o := range opts {
		o(t)
	}
	return t
}

// WithDecodeOptions sets the DecodeOptions AddFrame passes to DecodeAll,
// such as [WithoutInverted]. Since v2.8.
func WithDecodeOptions(opts ...DecodeOption) TrackOption {
	return func(t *FrameTracker) { t.opts = opts }
}

// SymbolTrack is one physical symbol followed across frames. Since v2.8.
type SymbolTrack struct {
	// Text is the symbol's payload, the same in every frame of the track.
	Text string
	// FirstFrame is the index, from 0, of the first frame the symbol was
	// read in.
	FirstFrame int
	// LastFrame is the index of the most recent frame it was read in.
	LastFrame int
	// Count is the number of frames in which the symbol was read.
	Count int
	// Best is the best decode of the symbol: the one that needed the least
	// error correction, preferring a hard-decision read and then the
	// largest appearance.
	Best *DecodeResult
}

// symbolTrack is the mutable state behind a [SymbolTrack].
type symbolTrack struct {
	order      int // creation order, for stable output
	text       string
	first      int
	last       int
	count      int
	best       *DecodeResult
	bestScore  float64
	bestHard   bool
	bestRadius float64
	// cx, cy and radius locate the symbol from its latest appearance: its
	// quadrilateral's center and the distance from there to its farthest
	// corner, which bounds its size whatever its orientation.
	cx, cy, radius float64
}

// Frame returns the index of the next frame AddFrame or AddResults will
// number, from 0 before the first call: frames already observed number 0
// to Frame()-1. Since v2.8.
func (t *FrameTracker) Frame() int { return t.frame }

// Reset forgets every track and starts a new sequence. The frame index
// restarts at 0. Since v2.8.
func (t *FrameTracker) Reset() {
	t.frame = 0
	t.order = 0
	t.size = image.Point{}
	t.tracks = nil
}

// AddFrame decodes every symbol in img with DecodeAll and folds them into
// the tracks. A frame in which nothing decodes counts as a frame and ages
// the existing tracks, ending those unseen for longer than the configured
// gap. A frame whose size differs from the previous one resets the
// tracks: positions from frames of another size cannot be compared.
//
// The only non-nil error is an invalid [TrackOption] or [DecodeOption]; a
// frame with no readable symbol is a normal empty frame, not an error. To
// bound the decode of each frame with a context, call DecodeAllContext
// yourself and feed its symbols to AddResults. It returns the tracks after
// the frame, as [FrameTracker.Tracks]. Since v2.8.
func (t *FrameTracker) AddFrame(img image.Image) ([]*SymbolTrack, error) {
	if t.err != nil {
		t.frame++
		return t.snapshot(), t.err
	}
	results, err := DecodeAll(img, t.opts...)
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrDecodeFailed) &&
		!errors.Is(err, ErrUnsupported) {
		// No symbols were read, so the frame only ages the tracks.
		t.frame++
		t.noteSize(img)
		return t.snapshot(), err
	}
	t.observe(results, img.Bounds().Size())
	return t.snapshot(), nil
}

// AddResults folds symbols decoded already into the tracks, numbering the
// frame from the tracker's frame index. It lets a caller keep its own
// decoding loop, for example one that searches a sub-image around the
// previous position first. Pass nil or an empty slice for a frame in
// which nothing decoded. The size is the decoded frame's size and must be
// in the same coordinates as the symbols' corners. Since v2.8.
func (t *FrameTracker) AddResults(results []*DecodeResult, size image.Point) ([]*SymbolTrack, error) {
	if t.err != nil {
		t.frame++
		return t.snapshot(), t.err
	}
	for i, r := range results {
		if r == nil {
			err := fmt.Errorf("%w: result %d is nil", ErrInvalidArgument, i)
			t.frame++
			return t.snapshot(), err
		}
	}
	t.observe(results, size)
	return t.snapshot(), nil
}

// noteSize records the frame size without resetting on an error path,
// where no symbols were observed anyway; the next successful frame of a
// different size then resets as usual.
func (t *FrameTracker) noteSize(img image.Image) {
	if t.size == (image.Point{}) {
		t.size = img.Bounds().Size()
	}
}

// observe is the shared update of AddFrame and AddResults.
func (t *FrameTracker) observe(results []*DecodeResult, size image.Point) {
	frame := t.frame
	t.frame++

	// A frame of another size cannot share positions with the frames
	// before it: start the tracks over, keeping the monotone frame index.
	if size != (image.Point{}) && t.size != (image.Point{}) && size != t.size {
		t.tracks = nil
	}
	if size != (image.Point{}) {
		t.size = size
	}

	// Retire tracks whose gap since their last appearance is exhausted.
	kept := t.tracks[:0]
	for _, tr := range t.tracks {
		if frame-tr.last <= t.maxGap {
			kept = append(kept, tr)
		}
	}
	t.tracks = kept

	if len(results) == 0 {
		return
	}

	type observation struct {
		res            *DecodeResult
		cx, cy, radius float64
		track          *symbolTrack
	}
	obs := make([]observation, len(results))
	for i, res := range results {
		cx, cy := quadCenter(res.Corners)
		obs[i] = observation{res: res, cx: cx, cy: cy, radius: quadRadius(res.Corners, cx, cy)}
	}

	// Match observations to tracks, best pair first, so that two symbols
	// of the same content cannot trade tracks. A pair needs the same text
	// and centers within 1.5 times the larger symbol's reach, which covers
	// the frame-to-frame motion of a camera while two distinct symbols a
	// symbol-width apart stay separate.
	type pair struct {
		oi, ti int
		d      float64
	}
	var pairs []pair
	for oi := range obs {
		for ti, tr := range t.tracks {
			if tr.text != obs[oi].res.Text {
				continue
			}
			reach := 1.5 * math.Max(obs[oi].radius, tr.radius)
			d := math.Hypot(obs[oi].cx-tr.cx, obs[oi].cy-tr.cy)
			if d <= reach {
				pairs = append(pairs, pair{oi, ti, d})
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].d != pairs[j].d {
			return pairs[i].d < pairs[j].d
		}
		if pairs[i].ti != pairs[j].ti {
			return pairs[i].ti < pairs[j].ti
		}
		return pairs[i].oi < pairs[j].oi
	})
	usedObs := make([]bool, len(obs))
	usedTrack := make([]bool, len(t.tracks))
	for _, p := range pairs {
		if usedObs[p.oi] || usedTrack[p.ti] {
			continue
		}
		usedObs[p.oi] = true
		usedTrack[p.ti] = true
		o := &obs[p.oi]
		o.track = t.tracks[p.ti]
	}

	// New tracks for observations that matched none, retiring the stalest
	// track first when the bound is reached.
	for oi := range obs {
		if usedObs[oi] {
			continue
		}
		if len(t.tracks) >= t.maxTracks {
			stale := 0
			for i := 1; i < len(t.tracks); i++ {
				if t.tracks[i].last < t.tracks[stale].last {
					stale = i
				}
			}
			t.tracks = append(t.tracks[:stale], t.tracks[stale+1:]...)
		}
		o := &obs[oi]
		tr := &symbolTrack{
			order: t.order,
			text:  o.res.Text,
			first: frame,
		}
		t.order++
		o.track = tr
		t.tracks = append(t.tracks, tr)
	}

	// Apply each observation to its track.
	for i := range obs {
		o := &obs[i]
		tr := o.track
		tr.last = frame
		tr.count++
		tr.cx, tr.cy, tr.radius = o.cx, o.cy, o.radius
		if tr.best == nil || betterDecode(o.res, o.radius, tr) {
			tr.best = o.res
			tr.bestScore = o.res.UnusedErrorCorrection
			tr.bestHard = !o.res.SoftDecision
			tr.bestRadius = o.radius
		}
	}
}

// betterDecode reports whether res is a better representative than the
// track's current best: most unused error correction, then a hard-decision
// read, then the largest appearance. Ties keep the earlier decode.
func betterDecode(res *DecodeResult, radius float64, tr *symbolTrack) bool {
	if res.UnusedErrorCorrection != tr.bestScore {
		return res.UnusedErrorCorrection > tr.bestScore
	}
	if hard := !res.SoftDecision; hard != tr.bestHard {
		return hard
	}
	return radius > tr.bestRadius
}

// quadRadius returns the distance from a quadrilateral's center to its
// farthest corner.
func quadRadius(q [4]image.Point, cx, cy float64) float64 {
	var r float64
	for _, p := range q {
		r = math.Max(r, math.Hypot(float64(p.X)-cx, float64(p.Y)-cy))
	}
	return r
}

// Tracks returns a snapshot of the tracks seen so far, in the order each
// symbol first appeared: the order is stable as symbols move. Tracks whose
// gap expired or that were retired for the track bound are not included.
// Since v2.8.
func (t *FrameTracker) Tracks() []*SymbolTrack {
	if t.err != nil {
		return nil
	}
	return t.snapshot()
}

// snapshot copies the tracks into the stable output order: the order each
// track was created in, so a symbol moving about does not reorder them.
func (t *FrameTracker) snapshot() []*SymbolTrack {
	out := make([]*SymbolTrack, len(t.tracks))
	for i, tr := range t.tracks {
		out[i] = &SymbolTrack{
			Text:       tr.text,
			FirstFrame: tr.first,
			LastFrame:  tr.last,
			Count:      tr.count,
			Best:       tr.best,
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return t.tracks[i].order < t.tracks[j].order
	})
	return out
}
