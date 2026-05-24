package media

import (
	"errors"
	"fmt"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"
)

// muxFragmentedMP4 combines a video-only fragmented MP4 (videoPath) and an
// audio-only fragmented MP4 (audioPath) into a single fragmented MP4 at outPath
// holding both tracks. It replaces the former static-ffmpeg `-c copy` step with
// a pure-Go structural rewrite (no external binary, no re-encode).
//
// Reddit serves every v.redd.it DASH/CMAF representation as fragmented MP4 —
//
//	ftyp / moov(init) / sidx / [moof + mdat]… / mfra
//
// — where the video and audio files both number their single track track_id=1
// and every fragment uses the default-base-is-moof flag, so each moof's
// trun.data_offset is relative to that moof rather than to an absolute file
// position. The mux therefore:
//
//   - merges the two init moov boxes into one (video trak + audio trak + a
//     combined mvex), renumbering the audio track to a free id wherever it is
//     named (tkhd, trex, and every fragment's tfhd) so the two track_id=1
//     tracks no longer collide;
//   - concatenates the fragments (all video moof+mdat, then all audio
//     moof+mdat), reassigning each moof's sequence_number so they stay strictly
//     increasing across the merged file;
//   - drops sidx/mfra, whose offsets would no longer be valid (both are
//     optional indices a player can rebuild by scanning the moof chain).
//
// Because default-base-is-moof keeps data offsets moof-relative, a fragment
// stays self-consistent as long as its rewritten moof preserves the original
// byte size. The rewrite only ever touches fixed-width fields, so the size is
// preserved — and copyMoofRewritten asserts it, failing the whole mux (so the
// caller falls back to the silent copy) rather than emitting a file whose audio
// or video would read from the wrong bytes.
func muxFragmentedMP4(videoPath, audioPath, outPath string) error {
	vr, err := os.Open(videoPath)
	if err != nil {
		return fmt.Errorf("open video: %w", err)
	}
	defer vr.Close()
	ar, err := os.Open(audioPath)
	if err != nil {
		return fmt.Errorf("open audio: %w", err)
	}
	defer ar.Close()

	// Track ids and movie timescales come straight from the init boxes rather
	// than mp4.Probe, so a quirky sample table never blocks a mux that only
	// rewrites box headers.
	vTrackIDs, err := trackIDs(vr)
	if err != nil {
		return fmt.Errorf("video track ids: %w", err)
	}
	aTrackIDs, err := trackIDs(ar)
	if err != nil {
		return fmt.Errorf("audio track ids: %w", err)
	}
	if len(vTrackIDs) == 0 {
		return errors.New("video has no track")
	}
	if len(aTrackIDs) != 1 {
		return fmt.Errorf("expected exactly one audio track, got %d", len(aTrackIDs))
	}
	audioOldID := aTrackIDs[0]
	var maxV uint32
	for _, id := range vTrackIDs {
		if id > maxV {
			maxV = id
		}
	}
	audioNewID := maxV + 1
	nextTrackID := audioNewID + 1

	// Confirm both inputs are the fragmented MP4 the rewrite assumes; a
	// progressive file (no mvex / no fragments) would not mux correctly, so bail
	// and let the caller keep the silent copy.
	vMvex, err := mp4.ExtractBox(vr, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvex()})
	if err != nil {
		return err
	}
	if len(vMvex) == 0 {
		return errors.New("video is not fragmented MP4 (no moov/mvex)")
	}

	vTS, vDur, err := movieTimescaleDuration(vr)
	if err != nil {
		return fmt.Errorf("video mvhd: %w", err)
	}
	aTS, aDur, err := movieTimescaleDuration(ar)
	if err != nil {
		return fmt.Errorf("audio mvhd: %w", err)
	}
	movieDur := vDur
	if aTS > 0 && vTS > 0 {
		if aInV := aDur * uint64(vTS) / uint64(aTS); aInV > movieDur {
			movieDur = aInV
		}
	}

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer out.Close()
	w := mp4.NewWriter(out)

	// ftyp — copy the video's verbatim; its brands already advertise the
	// fragmented-MP4 profile the output uses.
	ftyps, err := mp4.ExtractBox(vr, nil, mp4.BoxPath{mp4.BoxTypeFtyp()})
	if err != nil {
		return err
	}
	if len(ftyps) == 0 {
		return errors.New("video has no ftyp box")
	}
	if err := w.CopyBox(vr, ftyps[0]); err != nil {
		return fmt.Errorf("copy ftyp: %w", err)
	}

	if err := writeMergedMoov(w, vr, ar, audioOldID, audioNewID, nextTrackID, movieDur, vTS, aTS); err != nil {
		return fmt.Errorf("write moov: %w", err)
	}

	// Fragments: all of the video's, then all of the audio's, with one running
	// sequence_number across both so the merged moof chain stays ordered.
	seq := uint32(1)
	copyFragments := func(r io.ReadSeeker, remapFrom, remapTo uint32) error {
		return forEachTopLevel(r, func(bi *mp4.BoxInfo) error {
			switch bi.Type {
			case mp4.BoxTypeMoof():
				s := seq
				seq++
				return copyMoofRewritten(w, r, bi, s, remapFrom, remapTo)
			case mp4.BoxTypeMdat():
				return w.CopyBox(r, bi)
			default:
				// ftyp / moov / sidx / mfra / free — not part of the merged
				// media timeline.
				return nil
			}
		})
	}
	if err := copyFragments(vr, 0, 0); err != nil {
		return fmt.Errorf("copy video fragments: %w", err)
	}
	if err := copyFragments(ar, audioOldID, audioNewID); err != nil {
		return fmt.Errorf("copy audio fragments: %w", err)
	}

	return out.Sync()
}

// trackIDs returns the track_id of every moov/trak/tkhd in r, in file order.
func trackIDs(r io.ReadSeeker) ([]uint32, error) {
	bips, err := mp4.ExtractBoxWithPayload(r, nil,
		mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeTkhd()})
	if err != nil {
		return nil, err
	}
	ids := make([]uint32, 0, len(bips))
	for _, b := range bips {
		ids = append(ids, b.Payload.(*mp4.Tkhd).TrackID)
	}
	return ids, nil
}

// movieTimescaleDuration reads the movie timescale and duration from moov/mvhd.
func movieTimescaleDuration(r io.ReadSeeker) (timescale uint32, duration uint64, err error) {
	bips, err := mp4.ExtractBoxWithPayload(r, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvhd()})
	if err != nil {
		return 0, 0, err
	}
	if len(bips) == 0 {
		return 0, 0, errors.New("mvhd not found")
	}
	mvhd := bips[0].Payload.(*mp4.Mvhd)
	if mvhd.GetVersion() == 0 {
		return mvhd.Timescale, uint64(mvhd.DurationV0), nil
	}
	return mvhd.Timescale, mvhd.DurationV1, nil
}

// writeMergedMoov writes a single init moov holding both tracks: the video
// moov's children verbatim (mvhd patched with the new track count/duration, the
// video trak as-is), with the audio trak inserted before mvex and the audio
// trex appended inside it — all audio track ids remapped to audioNewID.
func writeMergedMoov(w *mp4.Writer, vr, ar io.ReadSeeker,
	audioOldID, audioNewID, nextTrackID uint32, movieDur uint64, vTS, aTS uint32) error {

	vMoovs, err := mp4.ExtractBox(vr, nil, mp4.BoxPath{mp4.BoxTypeMoov()})
	if err != nil {
		return err
	}
	if len(vMoovs) == 0 {
		return errors.New("video has no moov box")
	}
	vMoov := vMoovs[0]

	aTraks, err := mp4.ExtractBox(ar, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()})
	if err != nil {
		return err
	}
	if len(aTraks) == 0 {
		return errors.New("audio moov has no trak")
	}
	aTrexes, err := mp4.ExtractBoxWithPayload(ar, nil,
		mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvex(), mp4.BoxTypeTrex()})
	if err != nil {
		return err
	}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}

	audioInserted := false
	insertAudioTrak := func() error {
		if err := copyTrakRemapped(w, ar, aTraks[0], audioNewID, aTS, vTS); err != nil {
			return err
		}
		audioInserted = true
		return nil
	}

	err = forEachChild(vr, vMoov, func(bi *mp4.BoxInfo) error {
		switch bi.Type {
		case mp4.BoxTypeMvhd():
			return writePatchedMvhd(w, vr, bi, nextTrackID, movieDur)
		case mp4.BoxTypeMvex():
			// trak boxes precede mvex; slot the audio trak in first.
			if !audioInserted {
				if err := insertAudioTrak(); err != nil {
					return err
				}
			}
			return writeMergedMvex(w, vr, bi, aTrexes, audioNewID)
		default:
			// video trak, udta, anything else — copy verbatim.
			return w.CopyBox(vr, bi)
		}
	})
	if err != nil {
		return err
	}
	if !audioInserted {
		return errors.New("video moov missing mvex; cannot merge tracks")
	}

	_, err = w.EndBox()
	return err
}

// writePatchedMvhd copies a movie header, resetting next_track_ID for the added
// track and widening the duration to cover whichever track runs longer.
func writePatchedMvhd(w *mp4.Writer, r io.ReadSeeker, bi *mp4.BoxInfo, nextTrackID uint32, movieDur uint64) error {
	var mvhd mp4.Mvhd
	if err := unmarshalBox(r, bi, &mvhd); err != nil {
		return err
	}
	mvhd.NextTrackID = nextTrackID
	if movieDur > 0 {
		if mvhd.GetVersion() == 0 {
			mvhd.DurationV0 = uint32(movieDur)
		} else {
			mvhd.DurationV1 = movieDur
		}
	}
	return writeBox(w, &mvhd)
}

// writeMergedMvex writes an mvex that keeps the video's children (mehd, the
// video trex) verbatim and appends the audio trex with its track id remapped.
func writeMergedMvex(w *mp4.Writer, vr io.ReadSeeker, vMvex *mp4.BoxInfo,
	aTrexes []*mp4.BoxInfoWithPayload, audioNewID uint32) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMvex()}); err != nil {
		return err
	}
	if err := forEachChild(vr, vMvex, func(bi *mp4.BoxInfo) error {
		return w.CopyBox(vr, bi)
	}); err != nil {
		return err
	}
	for _, b := range aTrexes {
		trex := b.Payload.(*mp4.Trex)
		trex.TrackID = audioNewID
		if err := writeBox(w, trex); err != nil {
			return err
		}
	}
	_, err := w.EndBox()
	return err
}

// copyTrakRemapped copies a trak, rewriting tkhd's track id (and rescaling its
// movie-timescale duration when the source movie timescale differs from the
// output's). Everything below tkhd — mdia and its sample-entry tree — is copied
// verbatim.
func copyTrakRemapped(w *mp4.Writer, r io.ReadSeeker, trak *mp4.BoxInfo, newID, srcTS, dstTS uint32) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	err := forEachChild(r, trak, func(bi *mp4.BoxInfo) error {
		if bi.Type != mp4.BoxTypeTkhd() {
			return w.CopyBox(r, bi)
		}
		var tkhd mp4.Tkhd
		if err := unmarshalBox(r, bi, &tkhd); err != nil {
			return err
		}
		tkhd.TrackID = newID
		if srcTS > 0 && dstTS > 0 && srcTS != dstTS {
			if tkhd.GetVersion() == 0 {
				tkhd.DurationV0 = uint32(uint64(tkhd.DurationV0) * uint64(dstTS) / uint64(srcTS))
			} else {
				tkhd.DurationV1 = tkhd.DurationV1 * uint64(dstTS) / uint64(srcTS)
			}
		}
		return writeBox(w, &tkhd)
	})
	if err != nil {
		return err
	}
	_, err = w.EndBox()
	return err
}

// copyMoofRewritten rewrites one movie fragment: mfhd.sequence_number becomes
// seq, and (when remapTo != 0) every traf/tfhd naming remapFrom is repointed to
// remapTo. Only fixed-width fields change, so the moof keeps its original byte
// size — required because the following trun.data_offset values are relative to
// the moof start. The size is asserted; a mismatch means a box did not
// round-trip and the data offsets can no longer be trusted, so the mux fails.
func copyMoofRewritten(w *mp4.Writer, r io.ReadSeeker, moof *mp4.BoxInfo, seq, remapFrom, remapTo uint32) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoof()}); err != nil {
		return err
	}
	err := forEachChild(r, moof, func(bi *mp4.BoxInfo) error {
		switch bi.Type {
		case mp4.BoxTypeMfhd():
			var mfhd mp4.Mfhd
			if err := unmarshalBox(r, bi, &mfhd); err != nil {
				return err
			}
			mfhd.SequenceNumber = seq
			return writeBox(w, &mfhd)
		case mp4.BoxTypeTraf():
			if remapTo != 0 {
				return copyTrafRemapped(w, r, bi, remapFrom, remapTo)
			}
			return w.CopyBox(r, bi)
		default:
			return w.CopyBox(r, bi)
		}
	})
	if err != nil {
		return err
	}
	end, err := w.EndBox()
	if err != nil {
		return err
	}
	if end.Size != moof.Size {
		return fmt.Errorf("moof size changed %d -> %d; data offsets would be invalid", moof.Size, end.Size)
	}
	return nil
}

// copyTrafRemapped copies a traf, rewriting tfhd's track id from remapFrom to
// remapTo. tfdt/trun/sample tables are copied verbatim; their data offsets are
// moof-relative and unaffected by the track-id change.
func copyTrafRemapped(w *mp4.Writer, r io.ReadSeeker, traf *mp4.BoxInfo, remapFrom, remapTo uint32) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTraf()}); err != nil {
		return err
	}
	err := forEachChild(r, traf, func(bi *mp4.BoxInfo) error {
		if bi.Type != mp4.BoxTypeTfhd() {
			return w.CopyBox(r, bi)
		}
		var tfhd mp4.Tfhd
		if err := unmarshalBox(r, bi, &tfhd); err != nil {
			return err
		}
		if remapFrom == 0 || tfhd.TrackID == remapFrom {
			tfhd.TrackID = remapTo
		}
		return writeBox(w, &tfhd)
	})
	if err != nil {
		return err
	}
	_, err = w.EndBox()
	return err
}

// forEachTopLevel walks the boxes at the root of r, invoking fn for each. fn may
// consume bytes (e.g. CopyBox); the cursor is restored to the box end before the
// next iteration.
func forEachTopLevel(r io.ReadSeeker, fn func(bi *mp4.BoxInfo) error) error {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for {
		bi, err := mp4.ReadBoxInfo(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(bi); err != nil {
			return err
		}
		if _, err := bi.SeekToEnd(r); err != nil {
			return err
		}
	}
}

// forEachChild walks the direct children of parent, invoking fn for each, then
// restoring the cursor to each child's end.
func forEachChild(r io.ReadSeeker, parent *mp4.BoxInfo, fn func(bi *mp4.BoxInfo) error) error {
	if _, err := parent.SeekToPayload(r); err != nil {
		return err
	}
	end := parent.Offset + parent.Size
	for {
		cur, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if uint64(cur) >= end {
			return nil
		}
		bi, err := mp4.ReadBoxInfo(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(bi); err != nil {
			return err
		}
		if _, err := bi.SeekToEnd(r); err != nil {
			return err
		}
	}
}

// unmarshalBox parses a single box's payload at bi into box.
func unmarshalBox(r io.ReadSeeker, bi *mp4.BoxInfo, box mp4.IBox) error {
	if _, err := bi.SeekToPayload(r); err != nil {
		return err
	}
	_, err := mp4.Unmarshal(r, bi.Size-bi.HeaderSize, box, bi.Context)
	return err
}

// writeBox writes box as a complete box (header + marshalled payload), letting
// the Writer back-patch the size.
func writeBox(w *mp4.Writer, box mp4.IBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}
