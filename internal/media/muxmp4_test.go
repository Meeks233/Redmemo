package media

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

// fragSpec describes one synthetic fragmented-MP4 input: a single track and a
// list of per-fragment mdat payloads.
type fragSpec struct {
	trackID   uint32
	movieTS   uint32
	mediaTS   uint32
	duration  uint32
	sampleDur uint32 // per-fragment sample duration (media timescale); 0 = omit
	fragments [][]byte
}

// trunDataOffset is the placeholder moof-relative data offset baked into every
// synthetic fragment. The mux copies trun verbatim, so the value must survive
// unchanged — the test asserts that, which proves trun (and the moof's byte
// size) was preserved.
const trunDataOffset = 96

func box(w *mp4.Writer, typ mp4.BoxType, inner func() error) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: typ}); err != nil {
		return err
	}
	if err := inner(); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

// buildFragMP4 writes a minimal but well-formed fragmented MP4 to path:
// ftyp / moov(mvhd, trak(tkhd, mdia(mdhd)), mvex(trex)) / [moof + mdat]…
func buildFragMP4(t *testing.T, path string, spec fragSpec) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := mp4.NewWriter(f)

	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', '5'},
		MinorVersion: 0,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}
	if err := writeBox(w, ftyp); err != nil {
		t.Fatal(err)
	}

	err = box(w, mp4.BoxTypeMoov(), func() error {
		if err := writeBox(w, &mp4.Mvhd{Timescale: spec.movieTS, DurationV0: spec.duration, NextTrackID: 2}); err != nil {
			return err
		}
		if err := box(w, mp4.BoxTypeTrak(), func() error {
			if err := writeBox(w, &mp4.Tkhd{TrackID: spec.trackID, DurationV0: spec.duration}); err != nil {
				return err
			}
			return box(w, mp4.BoxTypeMdia(), func() error {
				if err := writeBox(w, &mp4.Mdhd{Timescale: spec.mediaTS, DurationV0: spec.duration}); err != nil {
					return err
				}
				// Empty sample tables — the real samples live in the fragments,
				// but mp4.Probe needs stbl present to parse the track.
				return box(w, mp4.BoxTypeMinf(), func() error {
					return box(w, mp4.BoxTypeStbl(), func() error {
						if err := writeBox(w, &mp4.Stsd{EntryCount: 0}); err != nil {
							return err
						}
						if err := writeBox(w, &mp4.Stts{EntryCount: 0}); err != nil {
							return err
						}
						if err := writeBox(w, &mp4.Stsc{EntryCount: 0}); err != nil {
							return err
						}
						if err := writeBox(w, &mp4.Stsz{SampleCount: 0}); err != nil {
							return err
						}
						return writeBox(w, &mp4.Stco{EntryCount: 0})
					})
				})
			})
		}); err != nil {
			return err
		}
		return box(w, mp4.BoxTypeMvex(), func() error {
			return writeBox(w, &mp4.Trex{TrackID: spec.trackID, DefaultSampleDescriptionIndex: 1})
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	var baseDecode uint32
	for i, payload := range spec.fragments {
		seq := uint32(i + 1)
		bd := baseDecode
		err := box(w, mp4.BoxTypeMoof(), func() error {
			if err := writeBox(w, &mp4.Mfhd{SequenceNumber: seq}); err != nil {
				return err
			}
			return box(w, mp4.BoxTypeTraf(), func() error {
				tfhd := &mp4.Tfhd{TrackID: spec.trackID}
				tfhd.SetFlags(mp4.TfhdDefaultBaseIsMoof)
				if err := writeBox(w, tfhd); err != nil {
					return err
				}
				if err := writeBox(w, &mp4.Tfdt{BaseMediaDecodeTimeV0: bd}); err != nil {
					return err
				}
				entry := mp4.TrunEntry{SampleSize: uint32(len(payload))}
				flags := uint32(0x000001 | 0x000200) // data-offset + sample-size present
				if spec.sampleDur > 0 {
					entry.SampleDuration = spec.sampleDur
					flags |= 0x000100 // sample-duration present
				}
				trun := &mp4.Trun{
					SampleCount: 1,
					DataOffset:  trunDataOffset,
					Entries:     []mp4.TrunEntry{entry},
				}
				trun.SetFlags(flags)
				return writeBox(w, trun)
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeBox(w, &mp4.Mdat{Data: payload}); err != nil {
			t.Fatal(err)
		}
		baseDecode += uint32(len(payload))
	}
}

func TestMuxFragmentedMP4(t *testing.T) {
	dir := t.TempDir()
	videoPath := filepath.Join(dir, "v.mp4")
	audioPath := filepath.Join(dir, "a.mp4")
	outPath := filepath.Join(dir, "out.mp4")

	// trunDataOffset (96) bakes in a specific moof byte size, so leave
	// sampleDur unset here — adding the per-sample-duration field would push
	// the trun (and thus the moof) past 96 bytes and break the fixture's
	// hardcoded data-offset alignment.
	buildFragMP4(t, videoPath, fragSpec{
		trackID: 1, movieTS: 1000, mediaTS: 3000, duration: 2000,
		fragments: [][]byte{[]byte("VIDEO-FRAG-1"), []byte("VIDEO-FRAG-TWO")},
	})
	buildFragMP4(t, audioPath, fragSpec{
		trackID: 1, movieTS: 1000, mediaTS: 48000, duration: 2100,
		fragments: [][]byte{[]byte("AUDIO-A"), []byte("AUDIO-BB")},
	})

	if err := muxFragmentedMP4(videoPath, audioPath, outPath); err != nil {
		t.Fatalf("mux: %v", err)
	}

	out, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	// Output is PROGRESSIVE: ftyp / mdat / moov, with no fragmentation. The
	// progressive layout is required for compatibility with VLC's native mp4
	// demuxer and Telegram desktop's libavformat-derivative player.
	moofs, err := mp4.ExtractBox(out, nil, mp4.BoxPath{mp4.BoxTypeMoof()})
	if err != nil {
		t.Fatal(err)
	}
	if len(moofs) != 0 {
		t.Fatalf("progressive output must contain no moof boxes, got %d", len(moofs))
	}

	// One trak per track, with the source video as id 1 and audio renumbered
	// to id 2 to avoid the collision both source files would otherwise have
	// (both arrive with their own track_id=1).
	tkhds, err := mp4.ExtractBoxWithPayload(out, nil,
		mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeTkhd()})
	if err != nil {
		t.Fatal(err)
	}
	if len(tkhds) != 2 {
		t.Fatalf("trak count = %d, want 2", len(tkhds))
	}
	if got := tkhds[0].Payload.(*mp4.Tkhd).TrackID; got != 1 {
		t.Errorf("video trak id = %d, want 1", got)
	}
	if got := tkhds[1].Payload.(*mp4.Tkhd).TrackID; got != 2 {
		t.Errorf("audio trak id = %d, want 2", got)
	}

	// stsz lists every sample's size — both video fragments produced one
	// sample each, both audio fragments did too, so each track gets two
	// entries matching the input payload sizes.
	stszs, err := mp4.ExtractBoxWithPayload(out, nil,
		mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()})
	if err != nil || len(stszs) != 2 {
		t.Fatalf("stsz extraction err=%v, count=%d, want 2", err, len(stszs))
	}
	wantVideoSizes := []uint32{uint32(len("VIDEO-FRAG-1")), uint32(len("VIDEO-FRAG-TWO"))}
	wantAudioSizes := []uint32{uint32(len("AUDIO-A")), uint32(len("AUDIO-BB"))}
	gotVideoSizes := stszs[0].Payload.(*mp4.Stsz).EntrySize
	gotAudioSizes := stszs[1].Payload.(*mp4.Stsz).EntrySize
	if !equalU32Slice(gotVideoSizes, wantVideoSizes) {
		t.Errorf("video stsz = %v, want %v", gotVideoSizes, wantVideoSizes)
	}
	if !equalU32Slice(gotAudioSizes, wantAudioSizes) {
		t.Errorf("audio stsz = %v, want %v", gotAudioSizes, wantAudioSizes)
	}

	// co64 carries one absolute chunk offset per source fragment — two per
	// track — so the moov can locate each chunk's bytes inside the mdat.
	co64s, err := mp4.ExtractBoxWithPayload(out, nil,
		mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeCo64()})
	if err != nil || len(co64s) != 2 {
		t.Fatalf("co64 extraction err=%v, count=%d, want 2", err, len(co64s))
	}
	if got := len(co64s[0].Payload.(*mp4.Co64).ChunkOffset); got != 2 {
		t.Errorf("video chunk offsets = %d, want 2", got)
	}
	if got := len(co64s[1].Payload.(*mp4.Co64).ChunkOffset); got != 2 {
		t.Errorf("audio chunk offsets = %d, want 2", got)
	}

	// Sample bytes land in the mdat in fragment-interleaved order:
	//   video[0], audio[0], video[1], audio[1]
	// so a strict progressive reader (libavformat / VLC native mp4) reaches
	// audio packets without first walking past the whole video track.
	mdatBytesByteOrder, err := readMdatBytes(outPath)
	if err != nil {
		t.Fatal(err)
	}
	wantConcat := "VIDEO-FRAG-1" + "AUDIO-A" + "VIDEO-FRAG-TWO" + "AUDIO-BB"
	if string(mdatBytesByteOrder) != wantConcat {
		t.Errorf("mdat byte order = %q, want %q", string(mdatBytesByteOrder), wantConcat)
	}
}

func equalU32Slice(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readMdatBytes returns the payload bytes of the (single) mdat box in the
// progressive output. We bypass mp4.ExtractBoxWithPayload because the writer
// emits a large-size (16-byte header) mdat, and we want raw bytes regardless
// of header form.
func readMdatBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bis, err := mp4.ExtractBox(f, nil, mp4.BoxPath{mp4.BoxTypeMdat()})
	if err != nil {
		return nil, err
	}
	if len(bis) != 1 {
		return nil, errors.New("expected exactly one mdat")
	}
	if _, err := bis[0].SeekToPayload(f); err != nil {
		return nil, err
	}
	payloadSize := bis[0].Size - bis[0].HeaderSize
	buf := make([]byte, payloadSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func TestMuxFragmentedMP4_RejectsNonFragmented(t *testing.T) {
	dir := t.TempDir()
	// A file with no moov/mvex must be rejected so the caller keeps the silent
	// copy rather than emitting a broken mux.
	plain := filepath.Join(dir, "plain.mp4")
	f, err := os.Create(plain)
	if err != nil {
		t.Fatal(err)
	}
	w := mp4.NewWriter(f)
	if err := writeBox(w, &mp4.Ftyp{MajorBrand: [4]byte{'m', 'p', '4', '2'}}); err != nil {
		t.Fatal(err)
	}
	if err := box(w, mp4.BoxTypeMoov(), func() error {
		return writeBox(w, &mp4.Mvhd{Timescale: 1000, NextTrackID: 2})
	}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	audio := filepath.Join(dir, "a.mp4")
	buildFragMP4(t, audio, fragSpec{
		trackID: 1, movieTS: 1000, mediaTS: 48000, duration: 100,
		fragments: [][]byte{[]byte("A")},
	})

	if err := muxFragmentedMP4(plain, audio, filepath.Join(dir, "out.mp4")); err == nil {
		t.Fatal("expected an error muxing a non-fragmented video, got nil")
	}
}

func TestAudioIsSilentPlaceholder(t *testing.T) {
	dir := t.TempDir()

	// Silent placeholder: tiny payloads over a long duration → ~4 kbps, well
	// under the 8 kbps threshold. 5 samples × 48 bytes over 5 × 48000 ticks at a
	// 48000 timescale = 5 s → 8*240/5 = 384 bps.
	silentPath := filepath.Join(dir, "silent.mp4")
	silentFrags := make([][]byte, 5)
	for i := range silentFrags {
		silentFrags[i] = make([]byte, 48)
	}
	buildFragMP4(t, silentPath, fragSpec{
		trackID: 1, movieTS: 1000, mediaTS: 48000, duration: 5000, sampleDur: 48000,
		fragments: silentFrags,
	})

	// Real audio: ~96 kbps over the same timeline (12000 bytes/sample).
	realPath := filepath.Join(dir, "real.mp4")
	realFrags := make([][]byte, 5)
	for i := range realFrags {
		realFrags[i] = make([]byte, 12000)
	}
	buildFragMP4(t, realPath, fragSpec{
		trackID: 1, movieTS: 1000, mediaTS: 48000, duration: 5000, sampleDur: 48000,
		fragments: realFrags,
	})

	silent, err := audioIsSilentPlaceholder(silentPath)
	if err != nil {
		t.Fatalf("silent probe: %v", err)
	}
	if !silent {
		t.Error("low-bitrate track should be detected as a silent placeholder")
	}

	real, err := audioIsSilentPlaceholder(realPath)
	if err != nil {
		t.Fatalf("real probe: %v", err)
	}
	if real {
		t.Error("normal-bitrate track must not be flagged as silent")
	}
}
