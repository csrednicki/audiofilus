package remote

import (
	"io"
	"math"
	"os"
	"sync/atomic"

	"github.com/juanvillacortac/audiofilus/internal/decode"
)

// Playing a song while it downloads, like Plexamp does: the decoder reads the
// .part file from the disk, and before each read it is checked that the
// download is far enough ahead of it. If it isn't, the song plays silence
// (as the radio does when buffering) until the rest arrives; the audio
// thread never waits for the network.

// partialMargin is how far ahead of the decoder the download must be (and
// how much has to have arrived to start): more than any decoder reads at
// once. A var so the tests can use a small file.
var partialMargin int64 = 1 << 20

// download is a song being downloaded to its .part file.
type download struct {
	file    string
	size    int64 // the whole file (Content-Length), -1 if the server doesn't say (a transcode)
	written atomic.Int64
	done    atomic.Bool // all of it is on the disk
	failed  atomic.Bool
}

// counter writes to the file and counts it in the download.
type counter struct {
	f  *os.File
	dl *download
}

func (c *counter) Write(b []byte) (int, error) {
	n, err := c.f.Write(b)
	c.dl.written.Add(int64(n))
	return n, err
}

// OpenPartial opens the remote song path while it downloads, once enough of
// it has arrived. false means not yet (or never: then it plays when the
// download ends, as before). A format whose index is at the end of the file
// (some M4A) fails to open here and waits for the whole file.
func (s *Store) OpenPartial(path string) (decode.Source, bool) {
	s.mu.Lock()
	dl := s.parts[path]
	s.mu.Unlock()
	if dl == nil || dl.failed.Load() || (!dl.done.Load() && dl.written.Load() < partialMargin) {
		return nil, false
	}
	f, err := openShared(dl.file)
	if err != nil {
		return nil, false
	}
	src, err := decode.OpenFile(f)
	if err != nil {
		return nil, false
	}
	m, _ := s.Track(path)
	return &partial{Source: src, f: f, dl: dl, pending: -1, seconds: m.Duration}, true
}

// partial is a Source over a file still downloading.
type partial struct {
	decode.Source
	f       *os.File
	dl      *download
	pending int64 // a seek waiting for its part of the file, -1 none
	seconds int   // the length the server gives, 0 if unknown
}

// Length is the decoder's, unless it disagrees with the server's by more
// than a second (the server's precision): an MP3 without a header has its length guessed from the
// file's size, and when it opened only the first megabyte was there (a
// 3-minute song showed 34 seconds).
func (p *partial) Length() int64 {
	n := p.Source.Length()
	rate := int64(p.SampleRate())
	if p.seconds <= 0 || rate <= 0 {
		return n
	}
	want := int64(p.seconds) * rate
	if n > 0 && max(n-want, want-n) <= rate {
		return n
	}
	return want
}

// ahead says whether the file is on the disk up to offset plus the margin.
func (p *partial) ahead(offset int64) bool {
	return p.dl.done.Load() || p.dl.written.Load() >= offset+partialMargin
}

// offsetOf estimates where frame is in the file; without the file's size
// (a transcode) it can't be known until it is all there.
func (p *partial) offsetOf(frame int64) int64 {
	n := p.Length()
	if n <= 0 || p.dl.size <= 0 {
		return math.MaxInt64 / 2
	}
	return int64(float64(frame) / float64(n) * float64(p.dl.size))
}

// Read decodes what is on the disk, or fills dst with silence while the
// download catches up.
func (p *partial) Read(dst []float32) (int, error) {
	if p.dl.failed.Load() && !p.dl.done.Load() {
		return 0, io.ErrUnexpectedEOF
	}
	if p.pending >= 0 {
		if !p.ahead(p.offsetOf(p.pending)) {
			return silence(dst)
		}
		frame := p.pending
		p.pending = -1
		if err := p.Source.SeekFrame(frame); err != nil {
			return 0, err
		}
	}
	if pos, err := p.f.Seek(0, io.SeekCurrent); err == nil && !p.ahead(pos) {
		return silence(dst)
	}
	return p.Source.Read(dst)
}

// silence fills dst with zeros, as the radio does while buffering.
func silence(dst []float32) (int, error) {
	n := len(dst) &^ 1
	clear(dst[:n])
	return n, nil
}

// SeekFrame jumps now if that part has arrived, or when it does.
func (p *partial) SeekFrame(frame int64) error {
	if p.ahead(p.offsetOf(frame)) {
		p.pending = -1
		return p.Source.SeekFrame(frame)
	}
	p.pending = frame
	return nil
}

// Position is where a waiting seek goes, so the bar shows it.
func (p *partial) Position() int64 {
	if p.pending >= 0 {
		return p.pending
	}
	return p.Source.Position()
}
