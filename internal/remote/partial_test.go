package remote

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/juanvillacortac/audiofilus/internal/decode"
)

// TestPartialPlayback plays a song while it "downloads": it starts with a
// quarter of the file, shows the whole song's length (from the server's duration), stops advancing (silence) where the data ends, carries on when
// the rest arrives, and reaches the same end as the whole file. The .part
// file is renamed while it is open, as the Store does (Windows refuses that
// without openShared).
func TestPartialPlayback(t *testing.T) {
	for _, name := range []string{"tone.wav", "tone.flac", "tone.mp3"} {
		t.Run(name, func(t *testing.T) { testPartial(t, "../decode/testdata/"+name) })
	}
}

func testPartial(t *testing.T, file string) {
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	old := partialMargin
	partialMargin = int64(len(data) / 32)
	defer func() { partialMargin = old }()
	whole, err := decode.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	want, rate := whole.Length(), int64(whole.SampleRate())
	whole.Close()

	dir := t.TempDir()
	tmp := filepath.Join(dir, "1.part")
	part := len(data) / 4 // a quarter: the MP3's own length guess is then well off
	if err := os.WriteFile(tmp, data[:part], 0o644); err != nil {
		t.Fatal(err)
	}
	dl := &download{file: tmp, size: int64(len(data))}
	dl.written.Store(int64(part))
	s := NewStore(dir, nil)
	s.parts["subsonic://x/1"] = dl
	if err := s.SaveMeta("x", map[string]Meta{"1": {Duration: int((want + rate/2) / rate)}}); err != nil {
		t.Fatal(err)
	}
	src, ok := s.OpenPartial("subsonic://x/1")
	if !ok {
		t.Fatal("not opened with part the file")
	}
	defer src.Close()
	// With a quarter of the file the length is still the whole song's (the
	// MP3's own guess would be about a quarter).
	if n := src.Length(); max(n-want, want-n) > rate {
		t.Errorf("length %d with a quarter of the file, want about %d", n, want)
	}
	buf := make([]float32, 1024)
	read := func(times int) {
		for range times {
			if _, err := src.Read(buf); err != nil && err != io.EOF {
				t.Fatal(err)
			}
		}
	}
	read(400)
	stalled := src.Position()
	read(50)
	if src.Position() != stalled || stalled <= 0 || stalled >= want {
		t.Fatalf("position %d then %d with part the file (of %d)", stalled, src.Position(), want)
	}
	f, err := os.OpenFile(tmp, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(data[part:])
	f.Close()
	dl.written.Store(int64(len(data)))
	dl.done.Store(true)
	if err := os.Rename(tmp, filepath.Join(dir, "1"+filepath.Ext(file))); err != nil {
		t.Fatalf("renaming the open .part: %v", err)
	}
	for range 10000 {
		if _, err := src.Read(buf); err == io.EOF {
			break
		}
	}
	if src.Position() != want {
		t.Errorf("ended at %d, want %d", src.Position(), want)
	}
}
