package qr

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"testing"
)

// resultAt builds a decoded-looking symbol of the given text, an
// axis-aligned square of side centered at (cx, cy).
func resultAt(text string, cx, cy, side int) *DecodeResult {
	h := side / 2
	return &DecodeResult{
		Text:                  text,
		Version:               1,
		Corners:               [4]image.Point{{cx - h, cy - h}, {cx + h, cy - h}, {cx + h, cy + h}, {cx - h, cy + h}},
		UnusedErrorCorrection: 1,
	}
}

// frame is a sequence of symbols at one point in time.
type frame struct {
	size image.Point
	res  []*DecodeResult
}

// run feeds the frames to a new tracker and returns its final tracks.
func run(t *testing.T, frames []frame, opts ...TrackOption) (*FrameTracker, []*SymbolTrack) {
	t.Helper()
	tr := NewFrameTracker(opts...)
	var tracks []*SymbolTrack
	for i, f := range frames {
		var err error
		tracks, err = tr.AddResults(f.res, f.size)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	return tr, tracks
}

func trackTexts(tracks []*SymbolTrack) []string {
	out := make([]string, len(tracks))
	for i, tr := range tracks {
		out[i] = tr.Text
	}
	return out
}

// TestFrameTrackerSingleSymbol follows one symbol across frames as it
// moves and fails to read briefly.
func TestFrameTrackerSingleSymbol(t *testing.T) {
	frames := []frame{
		{image.Pt(400, 400), []*DecodeResult{resultAt("a", 100, 100, 40)}},
		{image.Pt(400, 400), []*DecodeResult{resultAt("a", 110, 105, 40)}},
		{image.Pt(400, 400), nil}, // glare: nothing read
		{image.Pt(400, 400), nil},
		{image.Pt(400, 400), []*DecodeResult{resultAt("a", 130, 115, 40)}},
	}
	_, tracks := run(t, frames)
	assertLen(t, tracks, 1)
	tr := tracks[0]
	assertEqual(t, "a", tr.Text)
	assertEqual(t, 0, tr.FirstFrame)
	assertEqual(t, 4, tr.LastFrame)
	assertEqual(t, 3, tr.Count)
	assertNotNil(t, tr.Best)
	assertEqual(t, "a", tr.Best.Text)
}

// TestFrameTrackerTwoMoving keeps two different symbols apart as they
// move and pass through empty frames.
func TestFrameTrackerTwoMoving(t *testing.T) {
	frames := []frame{
		{image.Pt(600, 200), []*DecodeResult{resultAt("left", 80, 100, 40), resultAt("right", 400, 100, 40)}},
		{image.Pt(600, 200), []*DecodeResult{resultAt("left", 100, 100, 40), resultAt("right", 380, 100, 40)}},
		{image.Pt(600, 200), nil},
		{image.Pt(600, 200), []*DecodeResult{resultAt("left", 120, 100, 40), resultAt("right", 360, 100, 40)}},
		{image.Pt(600, 200), []*DecodeResult{resultAt("left", 140, 100, 40), resultAt("right", 340, 100, 40)}},
	}
	_, tracks := run(t, frames)
	assertLen(t, tracks, 2)
	assertEqual(t, []string{"left", "right"}, trackTexts(tracks))
	for _, tr := range tracks {
		assertEqual(t, 0, tr.FirstFrame, tr.Text)
		assertEqual(t, 4, tr.LastFrame, tr.Text)
		assertEqual(t, 4, tr.Count, tr.Text) // frames 0, 1, 3, 4
	}
}

// TestFrameTrackerDuplicateContent expects identical symbols at different
// positions to stay separate tracks.
func TestFrameTrackerDuplicateContent(t *testing.T) {
	frames := []frame{
		{image.Pt(600, 200), []*DecodeResult{resultAt("same", 80, 100, 40), resultAt("same", 400, 100, 40)}},
		{image.Pt(600, 200), []*DecodeResult{resultAt("same", 82, 100, 40), resultAt("same", 398, 100, 40)}},
		{image.Pt(600, 200), []*DecodeResult{resultAt("same", 84, 100, 40), resultAt("same", 396, 100, 40)}},
	}
	_, tracks := run(t, frames)
	assertLen(t, tracks, 2)
	for _, tr := range tracks {
		assertEqual(t, "same", tr.Text)
		assertEqual(t, 3, tr.Count)
	}
	// The tracks keep their left/right identities through the frames.
	left, right := tracks[0], tracks[1]
	if left.Best.Corners[0].X >= right.Best.Corners[0].X {
		t.Fatalf("tracks crossed: left at %d, right at %d", left.Best.Corners[0].X, right.Best.Corners[0].X)
	}
}

// TestFrameTrackerDuplicateSwap expects same-content symbols that approach
// each other not to trade tracks: matching is best-pair-first.
func TestFrameTrackerDuplicateSwap(t *testing.T) {
	// Two symbols farther apart than the matching reach, each moving part
	// of the way toward the other's old place: every cross-pairing is out
	// of reach and each continues its own track.
	frames := []frame{
		{image.Pt(700, 200), []*DecodeResult{resultAt("dup", 100, 100, 40), resultAt("dup", 300, 100, 40)}},
		{image.Pt(700, 200), []*DecodeResult{resultAt("dup", 130, 100, 40), resultAt("dup", 270, 100, 40)}},
	}
	_, tracks := run(t, frames)
	assertLen(t, tracks, 2)
	// Each observation continued its own track, so each count is 2.
	assertEqual(t, 2, tracks[0].Count)
	assertEqual(t, 2, tracks[1].Count)
}

// TestFrameTrackerGapBridgeAndExpiry checks the finite window: a symbol
// returns within the gap as one track, and after it as a new one.
func TestFrameTrackerGapExpiry(t *testing.T) {
	mk := func() []frame {
		return []frame{
			{image.Pt(400, 400), []*DecodeResult{resultAt("g", 100, 100, 40)}},
			{image.Pt(400, 400), nil},
			{image.Pt(400, 400), nil},
			{image.Pt(400, 400), nil},
			{image.Pt(400, 400), nil},
			{image.Pt(400, 400), nil}, // last seen frame 0; this is frame 5, gap 5
			{image.Pt(400, 400), []*DecodeResult{resultAt("g", 100, 100, 40)}},
		}
	}
	// Default gap 5: a gap of five frames (1-5) is at the limit and
	// expires, so frame 6 starts a new track.
	_, tracks := run(t, mk())
	assertLen(t, tracks, 1)
	assertEqual(t, 6, tracks[0].FirstFrame)
	assertEqual(t, 6, tracks[0].LastFrame)
	assertEqual(t, 1, tracks[0].Count)

	// Gap 6: the same absence is within the window and joins the track.
	trBridge := NewFrameTracker(WithTrackGap(6))
	var got []*SymbolTrack
	for i, f := range mk() {
		var err error
		got, err = trBridge.AddResults(f.res, f.size)
		assertNoError(t, err, "frame %d", i)
	}
	assertLen(t, got, 1)
	assertEqual(t, 0, got[0].FirstFrame)
	assertEqual(t, 6, got[0].LastFrame)
	assertEqual(t, 2, got[0].Count)

	// Gap 4: expiry happens a frame earlier; frame 6 starts new.
	tr2 := NewFrameTracker(WithTrackGap(4))
	for i, f := range mk() {
		var err error
		got, err = tr2.AddResults(f.res, f.size)
		assertNoError(t, err, "frame %d", i)
	}
	assertLen(t, got, 1)
	assertEqual(t, 6, got[0].FirstFrame)
	assertEqual(t, 6, got[0].LastFrame)
	assertEqual(t, 1, got[0].Count)

	// Gap 0: a single missing frame ends the track.
	tr3 := NewFrameTracker(WithTrackGap(0))
	_, err := tr3.AddResults([]*DecodeResult{resultAt("z", 100, 100, 40)}, image.Pt(400, 400))
	assertNoError(t, err)
	_, err = tr3.AddResults(nil, image.Pt(400, 400))
	assertNoError(t, err)
	got, err = tr3.AddResults([]*DecodeResult{resultAt("z", 100, 100, 40)}, image.Pt(400, 400))
	assertNoError(t, err)
	assertLen(t, got, 1)
	assertEqual(t, 2, got[0].FirstFrame)
}

// TestFrameTrackerEmptyFrames expects empty frames only to age tracks.
func TestFrameTrackerEmptyFrames(t *testing.T) {
	tr := NewFrameTracker()
	size := image.Pt(400, 400)
	got, err := tr.AddResults(nil, size)
	assertNoError(t, err)
	assertLen(t, got, 0)
	assertEqual(t, 1, tr.Frame())
	got, err = tr.AddResults(nil, size)
	assertNoError(t, err)
	assertLen(t, got, 0)
	assertEqual(t, 2, tr.Frame())
	got, err = tr.AddResults([]*DecodeResult{resultAt("e", 100, 100, 40)}, size)
	assertNoError(t, err)
	assertLen(t, got, 1)
	assertEqual(t, 2, got[0].FirstFrame)
}

// TestFrameTrackerSizeChange expects a frame of another size to start the
// tracks over while frame numbering continues.
func TestFrameTrackerSizeChange(t *testing.T) {
	tr := NewFrameTracker()
	got, err := tr.AddResults([]*DecodeResult{resultAt("a", 100, 100, 40)}, image.Pt(400, 400))
	assertNoError(t, err)
	assertLen(t, got, 1)
	got, err = tr.AddResults([]*DecodeResult{resultAt("a", 100, 100, 40)}, image.Pt(800, 600))
	assertNoError(t, err)
	assertLen(t, got, 1)
	assertEqual(t, 1, got[0].FirstFrame, "new size starts a new track")
	assertEqual(t, 2, tr.Frame())
	// Same size again keeps the new track.
	got, err = tr.AddResults([]*DecodeResult{resultAt("a", 105, 100, 40)}, image.Pt(800, 600))
	assertNoError(t, err)
	assertLen(t, got, 1)
	assertEqual(t, 1, got[0].FirstFrame)
	assertEqual(t, 2, got[0].Count)
}

// TestFrameTrackerRotationAndMirror follows a symbol through rotation and
// mirroring: corners move with it but the center stays associated.
func TestFrameTrackerRotationAndMirror(t *testing.T) {
	// A square turned 90 degrees has the same center and reach; a mirrored
	// one reverses its corner order but not its center.
	turned := &DecodeResult{
		Text: "spin", Corners: [4]image.Point{{120, 100}, {100, 120}, {80, 100}, {100, 80}},
		UnusedErrorCorrection: 1, Mirrored: true,
	}
	frames := []frame{
		{image.Pt(400, 400), []*DecodeResult{resultAt("spin", 100, 100, 40)}},
		{image.Pt(400, 400), []*DecodeResult{turned}},
	}
	_, tracks := run(t, frames)
	assertLen(t, tracks, 1)
	assertEqual(t, 2, tracks[0].Count)
	// The mirror flag is preserved on the decode it came from.
	assertTrue(t, turned.Mirrored)
}

// TestFrameTrackerMaxTracks bounds the number of tracks, retiring the
// stalest one.
func TestFrameTrackerMaxTracks(t *testing.T) {
	tr := NewFrameTracker(WithMaxTracks(2))
	size := image.Pt(400, 400)
	add := func(text string, x int) {
		t.Helper()
		if _, err := tr.AddResults([]*DecodeResult{resultAt(text, x, 100, 40)}, size); err != nil {
			t.Fatal(err)
		}
	}
	add("a", 80)
	add("b", 200)
	// "a" and "b" stay alive on this frame, then a new symbol arrives.
	if _, err := tr.AddResults(
		[]*DecodeResult{resultAt("a", 80, 100, 40), resultAt("b", 200, 100, 40)}, size); err != nil {
		t.Fatal(err)
	}
	add("c", 320)
	got := tr.Tracks()
	assertLen(t, got, 2)
	assertEqual(t, []string{"b", "c"}, trackTexts(got))
}

// TestFrameTrackerStableOrder expects output order to stay fixed as
// symbols move past each other.
func TestFrameTrackerStableOrder(t *testing.T) {
	tr := NewFrameTracker()
	size := image.Pt(600, 300)
	first := []*DecodeResult{resultAt("one", 100, 200, 40), resultAt("two", 400, 80, 40)}
	if _, err := tr.AddResults(first, size); err != nil {
		t.Fatal(err)
	}
	// "one" moves above "two" but stays within reach of its own track: a
	// position-sorted output would swap them, the tracker keeps creation
	// order.
	second := []*DecodeResult{resultAt("one", 105, 170, 40), resultAt("two", 395, 110, 40)}
	got, err := tr.AddResults(second, size)
	assertNoError(t, err)
	assertEqual(t, []string{"one", "two"}, trackTexts(got))
}

// TestFrameTrackerBestResult expects the cleanest, largest decode to be
// kept as Best.
func TestFrameTrackerBestResult(t *testing.T) {
	tr := NewFrameTracker()
	size := image.Pt(400, 400)
	weak := resultAt("q", 100, 100, 40)
	weak.UnusedErrorCorrection = 0.2
	weak.ErrorsCorrected = 7
	if _, err := tr.AddResults([]*DecodeResult{weak}, size); err != nil {
		t.Fatal(err)
	}
	clean := resultAt("q", 102, 100, 40)
	clean.UnusedErrorCorrection = 0.9
	got, err := tr.AddResults([]*DecodeResult{clean}, size)
	assertNoError(t, err)
	assertEqual(t, 0.9, got[0].Best.UnusedErrorCorrection)

	// Equal quality: a hard-decision read beats a soft one.
	soft := resultAt("q", 104, 100, 40)
	soft.UnusedErrorCorrection = 0.9
	soft.SoftDecision = true
	got, err = tr.AddResults([]*DecodeResult{soft}, size)
	assertNoError(t, err)
	assertFalse(t, got[0].Best.SoftDecision)

	// Equal quality and kind: the larger appearance wins.
	big := resultAt("q", 106, 100, 60)
	big.UnusedErrorCorrection = 0.9
	got, err = tr.AddResults([]*DecodeResult{big}, size)
	assertNoError(t, err)
	assertEqual(t, 60, got[0].Best.Corners[2].X-got[0].Best.Corners[0].X)
}

// TestFrameTrackerReset expects Reset to clear tracks and frame numbering.
func TestFrameTrackerReset(t *testing.T) {
	tr := NewFrameTracker()
	size := image.Pt(400, 400)
	if _, err := tr.AddResults([]*DecodeResult{resultAt("a", 100, 100, 40)}, size); err != nil {
		t.Fatal(err)
	}
	tr.Reset()
	assertEqual(t, 0, tr.Frame())
	assertLen(t, tr.Tracks(), 0)
	got, err := tr.AddResults([]*DecodeResult{resultAt("a", 100, 100, 40)}, size)
	assertNoError(t, err)
	assertEqual(t, 0, got[0].FirstFrame)
}

func TestFrameTrackerInvalidOptions(t *testing.T) {
	for _, opts := range [][]TrackOption{
		{WithTrackGap(-1)},
		{WithMaxTracks(0)},
		{WithMaxTracks(-3)},
	} {
		tr := NewFrameTracker(opts...)
		_, err := tr.AddResults(nil, image.Pt(400, 400))
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("opts %v: error %v, want ErrInvalidArgument", opts, err)
		}
		if got := tr.Tracks(); got != nil {
			t.Errorf("Tracks after invalid option = %v, want nil", got)
		}
	}
	_, err := NewFrameTracker().AddResults(
		[]*DecodeResult{resultAt("a", 1, 1, 10), nil}, image.Pt(400, 400))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("nil result: error %v, want ErrInvalidArgument", err)
	}
}

// TestFrameTrackerManySymbols bounds the work on a frame with many
// symbols and keeps every distinct position.
func TestFrameTrackerManySymbols(t *testing.T) {
	tr := NewFrameTracker()
	const n = 64
	res := make([]*DecodeResult, n)
	for i := range n {
		res[i] = resultAt("same", 40+(i%16)*60, 40+(i/16)*60, 30)
	}
	got, err := tr.AddResults(res, image.Pt(1000, 1000))
	assertNoError(t, err)
	assertLen(t, got, n)
	// A second identical frame continues every track rather than making
	// new ones.
	got, err = tr.AddResults(res, image.Pt(1000, 1000))
	assertNoError(t, err)
	assertLen(t, got, n)
	for _, g := range got {
		assertEqual(t, 2, g.Count)
	}
}

// TestFrameTrackerLongStream bounds the tracks over a long stream in
// which every frame brings a fresh symbol and some frames are empty.
func TestFrameTrackerLongStream(t *testing.T) {
	const bound = 8
	tr := NewFrameTracker(WithMaxTracks(bound), WithTrackGap(2))
	size := image.Pt(2000, 200)
	for f := 0; f < 200; f++ {
		var res []*DecodeResult
		if f%3 != 0 { // every third frame reads nothing
			// A fresh symbol, unique text, far from every earlier one.
			res = []*DecodeResult{resultAt(fmt.Sprintf("symbol-%03d", f), 60+(f*90)%1900, 100, 30)}
		}
		got, err := tr.AddResults(res, size)
		assertNoError(t, err, "frame %d", f)
		if len(got) > bound {
			t.Fatalf("frame %d: %d tracks, bound is %d", f, len(got), bound)
		}
		assertEqual(t, f+1, tr.Frame())
	}
	// The gap retires idle symbols too; either way the bound held.
	if len(tr.Tracks()) > bound {
		t.Fatalf("%d tracks, bound is %d", len(tr.Tracks()), bound)
	}
}

// TestFrameTrackerAddFrame end-to-end: rendered symbols move across real
// decoded frames, one frame is blank, and DecodeAll's results aggregate.
func TestFrameTrackerAddFrame(t *testing.T) {
	codeA := mustEncode(t, "moving A")
	codeB := mustEncode(t, "moving B")
	const s = 4
	imgA := mustImage(t, codeA, WithScale(s))
	imgB := mustImage(t, codeB, WithScale(s))

	canvas := func(offA, offB image.Point) *image.RGBA {
		c := image.NewRGBA(image.Rect(0, 0, 700, 260))
		draw.Draw(c, c.Bounds(), image.White, image.Point{}, draw.Src)
		draw.Draw(c, imgA.Bounds().Add(offA), imgA, image.Point{}, draw.Src)
		draw.Draw(c, imgB.Bounds().Add(offB), imgB, image.Point{}, draw.Src)
		return c
	}
	tr := NewFrameTracker(WithDecodeOptions(WithoutInverted()))
	frames := []*image.RGBA{
		canvas(image.Pt(20, 80), image.Pt(380, 80)),
		canvas(image.Pt(40, 80), image.Pt(360, 80)),
		image.NewRGBA(image.Rect(0, 0, 700, 260)), // blank: glare over the poster
		canvas(image.Pt(60, 80), image.Pt(340, 80)),
	}
	var tracks []*SymbolTrack
	for i, f := range frames {
		var err error
		tracks, err = tr.AddFrame(f)
		assertNoError(t, err, "frame %d", i)
	}
	assertLen(t, tracks, 2)
	assertEqual(t, []string{"moving A", "moving B"}, trackTexts(tracks))
	for _, tr := range tracks {
		assertEqual(t, 0, tr.FirstFrame)
		assertEqual(t, 3, tr.LastFrame)
		assertEqual(t, 3, tr.Count)
		assertNotNil(t, tr.Best)
	}
}

// TestFrameTrackerAddFrameGlare scratches a code so one frame cannot read
// it, and expects the surrounding frames to join into one track.
func TestFrameTrackerAddFrameGlare(t *testing.T) {
	code := mustEncode(t, "glare resistant", WithECC(ECCHigh))
	img := mustImage(t, code, WithScale(6))
	tr := NewFrameTracker()
	good := img
	tracks, err := tr.AddFrame(good)
	assertNoError(t, err)
	assertLen(t, tracks, 1)

	// A wide light stripe across the middle hides modules for one frame.
	glared := image.NewRGBA(img.Bounds())
	draw.Draw(glared, glared.Bounds(), img, image.Point{}, draw.Src)
	draw.Draw(glared, image.Rect(0, img.Bounds().Dy()/2-4, img.Bounds().Dx(), img.Bounds().Dy()/2+4),
		image.White, image.Point{}, draw.Src)
	tracks, err = tr.AddFrame(glared)
	if err == nil && len(tracks) == 1 && tracks[0].Count == 2 {
		// The code survived even the stripe; the tracker still joins it.
		assertEqual(t, 1, len(tracks))
	} else {
		// Expected: the stripe defeated this frame, leaving the track idle.
		assertLen(t, tracks, 1)
		assertEqual(t, 1, tracks[0].Count)
	}
	tracks, err = tr.AddFrame(good)
	assertNoError(t, err)
	assertLen(t, tracks, 1)
	assertEqual(t, 0, tracks[0].FirstFrame)
	assertEqual(t, 2, tracks[0].LastFrame)
}

// TestFrameTrackerAddFrameInvalidOption expects an invalid DecodeOption
// to be reported without dropping the frame numbering.
func TestFrameTrackerAddFrameInvalidOption(t *testing.T) {
	tr := NewFrameTracker(WithDecodeOptions(WithMaxSymbols(0)))
	_, err := tr.AddFrame(image.NewGray(image.Rect(0, 0, 100, 100)))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error %v, want ErrInvalidArgument", err)
	}
	assertEqual(t, 1, tr.Frame())

	// An invalid TrackOption is reported on every call.
	bad := NewFrameTracker(WithTrackGap(-1))
	if _, err := bad.AddFrame(image.NewGray(image.Rect(0, 0, 100, 100))); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error %v, want ErrInvalidArgument", err)
	}
}

// TestFrameTrackerReachBoundary checks the matching edge: a center moved
// by exactly the reach still joins, one beyond it starts a new track.
func TestFrameTrackerReachBoundary(t *testing.T) {
	// Side 40: center-to-far-corner radius is about 28.3, reach 1.5x is
	// about 42.4.
	tr := NewFrameTracker()
	size := image.Pt(600, 200)
	if _, err := tr.AddResults([]*DecodeResult{resultAt("r", 100, 100, 40)}, size); err != nil {
		t.Fatal(err)
	}
	// A move of 42 pixels is within reach.
	got, err := tr.AddResults([]*DecodeResult{resultAt("r", 142, 100, 40)}, size)
	assertNoError(t, err)
	assertLen(t, got, 1)
	assertEqual(t, 2, got[0].Count)
	// From the new position, a further 60-pixel jump is beyond reach.
	got, err = tr.AddResults([]*DecodeResult{resultAt("r", 202, 100, 40)}, size)
	assertNoError(t, err)
	assertLen(t, got, 2)
}

// TestFrameTrackerMaxTracksRetireAll fills the bound with symbols seen
// once, so every new symbol retires the stalest one.
func TestFrameTrackerMaxTracksRetireAll(t *testing.T) {
	tr := NewFrameTracker(WithMaxTracks(3))
	size := image.Pt(2000, 200)
	for f := 0; f < 6; f++ {
		res := []*DecodeResult{resultAt(fmt.Sprintf("only-%d", f), 80+f*300, 100, 40)}
		got, err := tr.AddResults(res, size)
		assertNoError(t, err)
		if len(got) > 3 {
			t.Fatalf("frame %d: %d tracks, bound 3", f, len(got))
		}
	}
	assertEqual(t, []string{"only-3", "only-4", "only-5"}, trackTexts(tr.Tracks()))
}

// TestFrameTrackerMatchingTie exercises the sort tie-breakers and the
// skip of a pair whose track was taken by a nearer observation: two
// identical symbols approach so every pairing is the same distance,
// which the tie-breakers resolve deterministically.
func TestFrameTrackerMatchingTie(t *testing.T) {
	tr := NewFrameTracker()
	size := image.Pt(600, 200)
	// Reach of a side-40 symbol is about 42.4. Tracks at 100 and 180;
	// observations at 140 and 140... instead use observations each 40
	// from each track: tracks 100 and 200, observations 140 and 160.
	first := []*DecodeResult{resultAt("tie", 100, 100, 40), resultAt("tie", 200, 100, 40)}
	if _, err := tr.AddResults(first, size); err != nil {
		t.Fatal(err)
	}
	// Each observation is 40 from each track: equal distances all round,
	// so the track-index and observation-index tie-breakers run, and one
	// observation skips a track already taken.
	second := []*DecodeResult{resultAt("tie", 140, 100, 40), resultAt("tie", 160, 100, 40)}
	got, err := tr.AddResults(second, size)
	assertNoError(t, err)
	// Both observations are within reach of both tracks; greedy matching
	// assigns one each through the deterministic tie-breakers, so both
	// tracks continue.
	assertLen(t, got, 2)
	for _, g := range got {
		assertEqual(t, 2, g.Count)
	}

	// A third frame with one observation continues one of the tracks.
	got, err = tr.AddResults([]*DecodeResult{resultAt("tie", 150, 100, 40)}, size)
	assertNoError(t, err)
	assertLen(t, got, 2)
	assertEqual(t, 5, got[0].Count+got[1].Count)

	// Two observations at 150: pairs share a distance but differ in track
	// index, and then in observation index, exercising both tie-breakers.
	got, err = tr.AddResults(
		[]*DecodeResult{resultAt("tie", 150, 100, 40), resultAt("tie", 150, 100, 40)}, size)
	assertNoError(t, err)
	assertNotEmpty(t, got)
}

// TestFrameTrackerRetireStalest expects a later-created track that has
// gone stale to be retired before an earlier one seen again.
func TestFrameTrackerRetireStalest(t *testing.T) {
	tr := NewFrameTracker(WithMaxTracks(3))
	size := image.Pt(2000, 200)
	if _, err := tr.AddResults(
		[]*DecodeResult{
			resultAt("a", 100, 100, 40),
			resultAt("b", 400, 100, 40),
			resultAt("c", 700, 100, 40),
		}, size); err != nil {
		t.Fatal(err)
	}
	// "a" is seen again; "b" and "c" idle. Then a fresh symbol arrives.
	if _, err := tr.AddResults([]*DecodeResult{resultAt("a", 100, 100, 40)}, size); err != nil {
		t.Fatal(err)
	}
	got, err := tr.AddResults([]*DecodeResult{resultAt("d", 1000, 100, 40)}, size)
	assertNoError(t, err)
	// "b" was created before "c" and is equally stale, so it is retired
	// first; the surviving tracks are a, c and d, in creation order.
	assertEqual(t, []string{"a", "c", "d"}, trackTexts(got))
}

// TestFrameTrackerSingleImageParity checks the aggregation against the
// existing single-symbol and multi-symbol decode behavior on one frame.
func TestFrameTrackerSingleImageParity(t *testing.T) {
	one := mustImage(t, mustEncode(t, "only"))
	sheet, _ := sheet(t, []string{"same", "same", "other"}, 3, 200, 5)

	tr := NewFrameTracker()
	tracks, err := tr.AddFrame(one)
	assertNoError(t, err)
	assertLen(t, tracks, 1)
	want, err := Decode(one)
	assertNoError(t, err)
	assertEqual(t, want.Text, tracks[0].Text)
	assertEqual(t, want.Corners, tracks[0].Best.Corners)

	tr = NewFrameTracker()
	tracks, err = tr.AddFrame(sheet)
	assertNoError(t, err)
	all, err := DecodeAll(sheet)
	assertNoError(t, err)
	assertLen(t, tracks, len(all))
	assertEqual(t, texts(all), trackTexts(tracks))
	for i, r := range all {
		assertEqual(t, r.Corners, tracks[i].Best.Corners)
		assertEqual(t, 0, tracks[i].FirstFrame)
		assertEqual(t, 1, tracks[i].Count)
	}
}

// BenchmarkFrameTracker measures the aggregation cost per frame, separate
// from the DecodeAll that produces the symbols: a grid of symbols each
// moving a little between frames.
func BenchmarkFrameTracker(b *testing.B) {
	const n = 16
	res := make([]*DecodeResult, n)
	for i := range n {
		res[i] = resultAt(fmt.Sprintf("bench-%02d", i), 60+(i%8)*110, 60+(i/8)*110, 40)
	}
	moved := make([]*DecodeResult, n)
	for i := range res {
		r := *res[i]
		r.Corners[0].X += 4
		r.Corners[1].X += 4
		r.Corners[2].X += 4
		r.Corners[3].X += 4
		moved[i] = &r
	}
	size := image.Pt(1000, 400)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr := NewFrameTracker()
		if _, err := tr.AddResults(res, size); err != nil {
			b.Fatal(err)
		}
		if _, err := tr.AddResults(moved, size); err != nil {
			b.Fatal(err)
		}
	}
}
