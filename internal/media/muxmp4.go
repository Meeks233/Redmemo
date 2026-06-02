package media

import (
	"errors"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"
)

// silentAudioBitrateThreshold is the bits/sec below which an audio track is
// taken to be one of Reddit's silent placeholders. Reddit generates a ~4 kbps
// AAC track for videos uploaded without sound (the DASH manifest lists it at
// ~4068 bps, and both the "128k" and "64k" variants are byte-identical silence).
// Real AAC content runs tens of kbps or more, so 8 kbps sits with wide margin
// above the placeholder and well below anything audible.
const silentAudioBitrateThreshold = 8000

// audioIsSilentPlaceholder reports whether the fragmented audio MP4 at path is a
// silent placeholder rather than real audio, judged purely by measured bitrate
// (no decode). It returns false on any parse failure so an unreadable probe
// never suppresses a mux that might carry real sound.
func audioIsSilentPlaceholder(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	pi, err := mp4.Probe(f)
	if err != nil {
		return false, err
	}
	if len(pi.Tracks) == 0 {
		return false, nil
	}
	tr := pi.Tracks[0]
	if tr.Timescale == 0 {
		return false, nil
	}
	bitrate := pi.Segments.GetBitrate(tr.TrackID, tr.Timescale)
	if bitrate == 0 {
		// No fragment timing to measure — don't second-guess; let it mux.
		return false, nil
	}
	return bitrate < silentAudioBitrateThreshold, nil
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

// forEachTopLevel walks the boxes at the root of r, invoking fn for each. fn
// may consume bytes (e.g. CopyBox); the cursor is restored to the box end
// before the next iteration.
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
