package media

import (
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

func moofSizes(t *testing.T, path string) []uint64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bis, err := mp4.ExtractBox(f, nil, mp4.BoxPath{mp4.BoxTypeMoof()})
	if err != nil {
		t.Fatal(err)
	}
	sizes := make([]uint64, len(bis))
	for i, bi := range bis {
		sizes[i] = bi.Size
	}
	return sizes
}

func TestMuxFragmentedMP4(t *testing.T) {
	dir := t.TempDir()
	videoPath := filepath.Join(dir, "v.mp4")
	audioPath := filepath.Join(dir, "a.mp4")
	outPath := filepath.Join(dir, "out.mp4")

	buildFragMP4(t, videoPath, fragSpec{
		trackID: 1, movieTS: 1000, mediaTS: 3000, duration: 2000,
		fragments: [][]byte{[]byte("VIDEO-FRAG-1"), []byte("VIDEO-FRAG-TWO")},
	})
	buildFragMP4(t, audioPath, fragSpec{
		trackID: 1, movieTS: 1000, mediaTS: 48000, duration: 2100,
		fragments: [][]byte{[]byte("AUDIO-A"), []byte("AUDIO-BB")},
	})

	vMoofSizes := moofSizes(t, videoPath)
	aMoofSizes := moofSizes(t, audioPath)

	if err := muxFragmentedMP4(videoPath, audioPath, outPath); err != nil {
		t.Fatalf("mux: %v", err)
	}

	out, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	// Two tracks, video keeps id 1 and audio is renumbered to 2.
	ids, err := trackIDs(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("track ids = %v, want [1 2]", ids)
	}

	// mvex carries a trex per track, audio remapped to 2.
	trexes, err := mp4.ExtractBoxWithPayload(out, nil,
		mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvex(), mp4.BoxTypeTrex()})
	if err != nil {
		t.Fatal(err)
	}
	var trexIDs []uint32
	for _, b := range trexes {
		trexIDs = append(trexIDs, b.Payload.(*mp4.Trex).TrackID)
	}
	if len(trexIDs) != 2 || trexIDs[0] != 1 || trexIDs[1] != 2 {
		t.Fatalf("trex ids = %v, want [1 2]", trexIDs)
	}

	// mvhd.next_track_ID advanced past the added track.
	mvhds, err := mp4.ExtractBoxWithPayload(out, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvhd()})
	if err != nil {
		t.Fatal(err)
	}
	if got := mvhds[0].Payload.(*mp4.Mvhd).NextTrackID; got != 3 {
		t.Errorf("next_track_ID = %d, want 3", got)
	}
	// Movie duration widened to cover the longer (audio) track: 2100 in the
	// 1000-Hz movie timescale (both inputs share it here).
	if got := mvhds[0].Payload.(*mp4.Mvhd).DurationV0; got != 2100 {
		t.Errorf("movie duration = %d, want 2100", got)
	}

	// Fragments: video first, then audio; sequence numbers reassigned 1..4;
	// tfhd track ids [1 1 2 2]; each moof keeps its source byte size; trun data
	// offset copied unchanged.
	out.Seek(0, 0)
	moofs, err := mp4.ExtractBox(out, nil, mp4.BoxPath{mp4.BoxTypeMoof()})
	if err != nil {
		t.Fatal(err)
	}
	wantMoofSizes := append(append([]uint64{}, vMoofSizes...), aMoofSizes...)
	if len(moofs) != len(wantMoofSizes) {
		t.Fatalf("moof count = %d, want %d", len(moofs), len(wantMoofSizes))
	}
	wantTfhd := []uint32{1, 1, 2, 2}
	for i, m := range moofs {
		if m.Size != wantMoofSizes[i] {
			t.Errorf("moof[%d] size = %d, want %d (preservation broken)", i, m.Size, wantMoofSizes[i])
		}
		mfhd, err := mp4.ExtractBoxWithPayload(out, m, mp4.BoxPath{mp4.BoxTypeMfhd()})
		if err != nil {
			t.Fatal(err)
		}
		if got := mfhd[0].Payload.(*mp4.Mfhd).SequenceNumber; got != uint32(i+1) {
			t.Errorf("moof[%d] sequence = %d, want %d", i, got, i+1)
		}
		tfhd, err := mp4.ExtractBoxWithPayload(out, m, mp4.BoxPath{mp4.BoxTypeTraf(), mp4.BoxTypeTfhd()})
		if err != nil {
			t.Fatal(err)
		}
		if got := tfhd[0].Payload.(*mp4.Tfhd).TrackID; got != wantTfhd[i] {
			t.Errorf("moof[%d] tfhd track id = %d, want %d", i, got, wantTfhd[i])
		}
		trun, err := mp4.ExtractBoxWithPayload(out, m, mp4.BoxPath{mp4.BoxTypeTraf(), mp4.BoxTypeTrun()})
		if err != nil {
			t.Fatal(err)
		}
		if got := trun[0].Payload.(*mp4.Trun).DataOffset; got != trunDataOffset {
			t.Errorf("moof[%d] trun data offset = %d, want %d", i, got, trunDataOffset)
		}
	}

	// mdat payloads survive intact and in order (video fragments, then audio).
	out.Seek(0, 0)
	mdats, err := mp4.ExtractBoxWithPayload(out, nil, mp4.BoxPath{mp4.BoxTypeMdat()})
	if err != nil {
		t.Fatal(err)
	}
	wantData := []string{"VIDEO-FRAG-1", "VIDEO-FRAG-TWO", "AUDIO-A", "AUDIO-BB"}
	if len(mdats) != len(wantData) {
		t.Fatalf("mdat count = %d, want %d", len(mdats), len(wantData))
	}
	for i, b := range mdats {
		if got := string(b.Payload.(*mp4.Mdat).Data); got != wantData[i] {
			t.Errorf("mdat[%d] = %q, want %q", i, got, wantData[i])
		}
	}
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
