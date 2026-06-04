package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"
)

// muxFragmentedMP4 merges a fragmented video-only MP4 and a fragmented
// audio-only MP4 into a single PROGRESSIVE MP4 — ftyp / mdat / moov with one
// fully populated sample table per track. Reddit serves DASH/CMAF segments as
// fragmented MP4s, but multi-track fragmented MP4 is poorly supported outside
// browsers: VLC's native demuxer skips audio on interlaced/multi-track frags,
// and Telegram desktop (and other libavformat-derivative players) often drop
// the audio track entirely. A progressive MP4 with full stts/stsc/stsz/stco
// tables is the universal interchange format every player understands.
//
// Reddit DASH inputs are constrained: each file holds exactly one track with
// fragments using default-base-is-moof (so trun.data_offset is moof-relative),
// avc1/mp4a sample entries inside moov/trak/mdia/minf/stbl/stsd that the
// muxer copies verbatim. Audio is renumbered to track id 2 to avoid colliding
// with the video's track id 1.
//
// Output layout: ftyp / large mdat (samples grouped per source fragment,
// video[i] samples then audio[i] samples) / moov. moov is written last so the
// muxer can stream sample bytes directly from the source mdat into the output
// while recording each chunk's absolute byte position, then emit the sample
// table tail without a second pass.
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

	vInfo, err := readTrackInfo(vr)
	if err != nil {
		return fmt.Errorf("video track info: %w", err)
	}
	aInfo, err := readTrackInfo(ar)
	if err != nil {
		return fmt.Errorf("audio track info: %w", err)
	}

	vFrags, err := readFragmentSamples(vr)
	if err != nil {
		return fmt.Errorf("video samples: %w", err)
	}
	aFrags, err := readFragmentSamples(ar)
	if err != nil {
		return fmt.Errorf("audio samples: %w", err)
	}
	if len(vFrags) == 0 {
		return errors.New("video has no fragments")
	}
	if len(aFrags) == 0 {
		return errors.New("audio has no fragments")
	}

	// Renumber audio to track id 2 so it can't collide with video's track id 1.
	const (
		videoTrackID uint32 = 1
		audioTrackID uint32 = 2
	)
	vInfo.trackID = videoTrackID
	aInfo.trackID = audioTrackID

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer out.Close()

	// ftyp: emit a fresh progressive-MP4 ftyp. The source DASH/CMAF segment's
	// ftyp carries `dash`/`cmfc` brands; some browsers pick those up and try to
	// parse our output as fragmented MP4, stalling at ~4 s when they fail to
	// find a moof. See writeProgressiveFtyp for the rationale.
	if err := writeProgressiveFtyp(out); err != nil {
		return fmt.Errorf("write ftyp: %w", err)
	}

	// mdat: interleaved sample bytes, one chunk per source fragment so the
	// on-disk byte order roughly matches wall-clock playback order. Each chunk
	// records its absolute byte offset for the eventual stco/co64.
	vChunkOffsets, aChunkOffsets, mdatEnd, err := writeMdat(out, vr, ar, vFrags, aFrags)
	if err != nil {
		return fmt.Errorf("write mdat: %w", err)
	}

	// moov: progressive sample tables for both tracks. The sample tables refer
	// to the chunk offsets recorded above.
	if err := writeProgressiveMoov(out, vr, ar, vInfo, aInfo, vFrags, aFrags, vChunkOffsets, aChunkOffsets); err != nil {
		return fmt.Errorf("write moov: %w", err)
	}

	if _, err := out.Seek(int64(mdatEnd), io.SeekStart); err != nil {
		// Position-irrelevant — defer-close will flush. Keep going.
	}
	return out.Sync()
}

// trackInfo names per-track metadata pulled from a fragmented input's init
// moov. The original trak box is held by Offset/Size so the muxer can copy
// mdia children (hdlr / minf-vmhd-or-smhd / minf-dinf / minf-stbl-stsd) into
// the output without re-decoding them — those subtrees carry the codec
// configuration the output's stsd must reuse verbatim.
type trackInfo struct {
	trackID         uint32
	mediaTimescale  uint32
	movieTimescale  uint32
	mediaLanguage   [3]byte
	hdlrBox         *mp4.BoxInfo // mdia/hdlr — handler reference (vide / soun)
	stsdBox         *mp4.BoxInfo // mdia/minf/stbl/stsd — sample entries (avc1 / mp4a)
	mhdBox          *mp4.BoxInfo // mdia/minf/<vmhd|smhd|nmhd>
	dinfBox         *mp4.BoxInfo // mdia/minf/dinf — data reference
	trexSampleDur   uint32
	trexSampleSize  uint32
	trexSampleFlags uint32
	// tkhdWidth / tkhdHeight carry the source's per-track display dimensions
	// in 16.16 fixed-point. The output tkhd MUST copy these — emitting a
	// video tkhd with width/height = 0 makes Chrome render the <video> as a
	// zero-pixel box even when the avc1 sample entry has the correct frame
	// size, which matches the "video downloads but doesn't show" symptom.
	tkhdWidth  uint32
	tkhdHeight uint32
}

// sampleInfo names one decoded sample's location in the SOURCE file plus its
// timing/sync metadata. The mdat writer uses srcOffset+size to stream sample
// bytes; the moov builder uses size/duration/isSync/cto to build stbl.
type sampleInfo struct {
	srcOffset uint64
	size      uint32
	duration  uint32
	isSync    bool
	cto       int32
}

// fragSamples groups one source fragment's samples — they become one chunk in
// the output mdat, so stco/co64 holds exactly one offset per fragment and
// stsc carries one entry per fragment listing its sample count.
type fragSamples struct {
	samples []sampleInfo
}

// readTrackInfo extracts the per-track metadata the progressive output needs:
// timescales, language, trex defaults, and the source-file locations of the
// boxes the output's mdia/minf/stbl subtree will copy verbatim (stsd, hdlr,
// the per-handler media header, dinf).
func readTrackInfo(r io.ReadSeeker) (*trackInfo, error) {
	traks, err := mp4.ExtractBox(r, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()})
	if err != nil {
		return nil, err
	}
	if len(traks) != 1 {
		return nil, fmt.Errorf("expected exactly one trak, got %d", len(traks))
	}
	trak := traks[0]

	tkhds, err := mp4.ExtractBoxWithPayload(r, trak, mp4.BoxPath{mp4.BoxTypeTkhd()})
	if err != nil || len(tkhds) == 0 {
		return nil, fmt.Errorf("tkhd missing: %w", err)
	}
	srcTkhd := tkhds[0].Payload.(*mp4.Tkhd)
	originalTrackID := srcTkhd.TrackID

	mdhds, err := mp4.ExtractBoxWithPayload(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()})
	if err != nil || len(mdhds) == 0 {
		return nil, fmt.Errorf("mdhd missing: %w", err)
	}
	mdhd := mdhds[0].Payload.(*mp4.Mdhd)
	lang := mdhd.Language

	movieTS, _, err := movieTimescaleDuration(r)
	if err != nil {
		return nil, err
	}

	stsds, err := mp4.ExtractBox(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd()})
	if err != nil {
		return nil, err
	}
	hdlrs, err := mp4.ExtractBox(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()})
	if err != nil {
		return nil, err
	}
	dinfs, err := mp4.ExtractBox(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeDinf()})
	if err != nil {
		return nil, err
	}

	info := &trackInfo{
		trackID:        originalTrackID,
		mediaTimescale: mdhd.Timescale,
		movieTimescale: movieTS,
		mediaLanguage:  lang,
		tkhdWidth:      srcTkhd.Width,
		tkhdHeight:     srcTkhd.Height,
	}
	if len(stsds) > 0 {
		info.stsdBox = stsds[0]
	}
	if len(hdlrs) > 0 {
		info.hdlrBox = hdlrs[0]
	}
	if len(dinfs) > 0 {
		info.dinfBox = dinfs[0]
	}

	// Find media header — vmhd / smhd / nmhd / sthd, whichever is present.
	minfs, err := mp4.ExtractBox(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf()})
	if err != nil || len(minfs) == 0 {
		return nil, fmt.Errorf("minf missing: %w", err)
	}
	if err := forEachChild(r, minfs[0], func(bi *mp4.BoxInfo) error {
		switch bi.Type {
		case mp4.BoxTypeVmhd(), mp4.BoxTypeSmhd():
			info.mhdBox = bi
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// trex defaults — every fragmented track has one.
	trexes, err := mp4.ExtractBoxWithPayload(r, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvex(), mp4.BoxTypeTrex()})
	if err != nil {
		return nil, err
	}
	for _, b := range trexes {
		trex := b.Payload.(*mp4.Trex)
		if trex.TrackID == originalTrackID {
			info.trexSampleDur = trex.DefaultSampleDuration
			info.trexSampleSize = trex.DefaultSampleSize
			info.trexSampleFlags = trex.DefaultSampleFlags
			break
		}
	}
	return info, nil
}

// trexSampleDefaults captures the per-track defaults from a moov's trex —
// applied when a moof's tfhd does not override the corresponding flag.
type trexSampleDefaults struct {
	dur, size, flags uint32
}

// readFragmentSamples walks every moof+mdat pair in r and returns one
// fragSamples per moof, each carrying the absolute source-file offset, size,
// duration, sync flag, and composition-time-offset of every sample. Reddit
// fragments use default-base-is-moof, so trun.data_offset is moof-relative;
// each sample's source offset advances by its own size as we walk the trun.
func readFragmentSamples(r io.ReadSeeker) ([]fragSamples, error) {
	// trex defaults: needed when a moof's tfhd omits the corresponding flag.
	trexDefs := map[uint32]trexSampleDefaults{}
	trexes, err := mp4.ExtractBoxWithPayload(r, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvex(), mp4.BoxTypeTrex()})
	if err != nil {
		return nil, err
	}
	for _, b := range trexes {
		t := b.Payload.(*mp4.Trex)
		trexDefs[t.TrackID] = trexSampleDefaults{t.DefaultSampleDuration, t.DefaultSampleSize, t.DefaultSampleFlags}
	}

	var out []fragSamples
	err = forEachTopLevel(r, func(bi *mp4.BoxInfo) error {
		if bi.Type != mp4.BoxTypeMoof() {
			return nil
		}
		group, err := parseMoofIntoSamples(r, bi, trexDefs)
		if err != nil {
			return err
		}
		if group != nil {
			out = append(out, *group)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseMoofIntoSamples walks one moof, parses its traf children (tfhd/tfdt
// and one or more trun), and returns the fragment's full sample list with
// absolute source offsets. tfdt is unused — for Reddit DASH, samples are
// contiguous and the output's stts represents that directly.
func parseMoofIntoSamples(r io.ReadSeeker, moof *mp4.BoxInfo, trexDefs map[uint32]trexSampleDefaults) (*fragSamples, error) {
	var trafBox *mp4.BoxInfo
	if err := forEachChild(r, moof, func(c *mp4.BoxInfo) error {
		if c.Type == mp4.BoxTypeTraf() {
			trafBox = c
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if trafBox == nil {
		return nil, nil
	}

	var (
		tfhd     *mp4.Tfhd
		trunBoxes []*mp4.Trun
	)
	if err := forEachChild(r, trafBox, func(c *mp4.BoxInfo) error {
		switch c.Type {
		case mp4.BoxTypeTfhd():
			var h mp4.Tfhd
			if err := unmarshalBox(r, c, &h); err != nil {
				return err
			}
			tfhd = &h
		case mp4.BoxTypeTrun():
			var t mp4.Trun
			if err := unmarshalBox(r, c, &t); err != nil {
				return err
			}
			trunBoxes = append(trunBoxes, &t)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if tfhd == nil {
		return nil, errors.New("traf without tfhd")
	}

	tfhdFlags := tfhd.GetFlags()
	defs := trexDefs[tfhd.TrackID]
	defDur := defs.dur
	defSize := defs.size
	defFlags := defs.flags
	if tfhdFlags&mp4.TfhdDefaultSampleDurationPresent != 0 {
		defDur = tfhd.DefaultSampleDuration
	}
	if tfhdFlags&mp4.TfhdDefaultSampleSizePresent != 0 {
		defSize = tfhd.DefaultSampleSize
	}
	if tfhdFlags&mp4.TfhdDefaultSampleFlagsPresent != 0 {
		defFlags = tfhd.DefaultSampleFlags
	}

	// Base data offset: explicit if present, else moof.Offset when
	// default-base-is-moof is set (Reddit's DASH always does), else first byte
	// of file as a last resort. Reddit's fragments use default-base-is-moof
	// universally so the third branch is dead weight in practice.
	var base int64
	switch {
	case tfhdFlags&mp4.TfhdBaseDataOffsetPresent != 0:
		base = int64(tfhd.BaseDataOffset)
	case tfhdFlags&mp4.TfhdDefaultBaseIsMoof != 0:
		base = int64(moof.Offset)
	default:
		base = int64(moof.Offset)
	}

	var samples []sampleInfo
	for _, trun := range trunBoxes {
		flags := trun.GetFlags()
		offset := base
		if flags&0x000001 != 0 { // data-offset-present
			offset = base + int64(trun.DataOffset)
		}
		firstFlags := defFlags
		if flags&0x000004 != 0 {
			firstFlags = trun.FirstSampleFlags
		}
		for i, e := range trun.Entries {
			size := defSize
			if flags&0x000200 != 0 {
				size = e.SampleSize
			}
			dur := defDur
			if flags&0x000100 != 0 {
				dur = e.SampleDuration
			}
			sf := defFlags
			if i == 0 && flags&0x000004 != 0 {
				sf = firstFlags
			} else if flags&0x000400 != 0 {
				sf = e.SampleFlags
			}
			var cto int32
			if flags&0x000800 != 0 {
				if trun.GetVersion() == 0 {
					cto = int32(e.SampleCompositionTimeOffsetV0)
				} else {
					cto = e.SampleCompositionTimeOffsetV1
				}
			}
			// sample_is_non_sync_sample is bit 16 of sample_flags (MSB-first):
			// ISO 14496-12 §8.8.3.1 packs the bit so its mask is 0x00010000.
			isSync := sf&0x00010000 == 0
			samples = append(samples, sampleInfo{
				srcOffset: uint64(offset),
				size:      size,
				duration:  dur,
				isSync:    isSync,
				cto:       cto,
			})
			offset += int64(size)
		}
	}
	return &fragSamples{samples: samples}, nil
}

// writeProgressiveFtyp emits a fresh ftyp advertising progressive-MP4 brands
// only. Copying the source DASH/CMAF segment's ftyp verbatim drags along the
// `dash`/`cmfc` brands, and major browsers (Chrome/Edge confirmed) switch to
// fragmented-MP4 / MSE parsing on those brands — they then hunt for moof boxes
// after the first mdat and stall at the first chunk boundary (~4s of video)
// when none exist, because our output is progressive (mdat + tail moov). The
// brand set below mirrors what `ffmpeg -movflags +faststart` writes for plain
// AVC+AAC: `mp42` major, with `isom`/`mp41`/`mp42`/`avc1` compatibles. No
// `dash`, no `cmfc`, no `iso5/6` — every desktop and mobile player treats this
// as a progressive AVC/AAC container.
func writeProgressiveFtyp(out *os.File) error {
	const header = 8 // size(4) + type(4)
	brands := [][]byte{
		[]byte("isom"),
		[]byte("mp41"),
		[]byte("mp42"),
		[]byte("avc1"),
	}
	body := make([]byte, 0, 8+4*len(brands))
	body = append(body, []byte("mp42")...)        // major_brand
	body = append(body, 0x00, 0x00, 0x00, 0x00)   // minor_version
	for _, b := range brands {
		body = append(body, b...)
	}
	box := make([]byte, header+len(body))
	binary.BigEndian.PutUint32(box[0:4], uint32(header+len(body)))
	copy(box[4:8], []byte("ftyp"))
	copy(box[8:], body)
	_, err := out.Write(box)
	return err
}

// writeMdat emits a single large (64-bit) mdat holding all sample bytes,
// interleaved by source fragment (video[i] samples, audio[i] samples,
// video[i+1], …). It returns each track's per-fragment chunk offsets — the
// absolute byte position where that fragment's samples start in the output —
// so the moov writer can populate stco/co64.
//
// Using a 64-bit large-size mdat (header 16 bytes) sidesteps the 4 GiB ceiling
// of the 32-bit size field and keeps a single fixed header size whether the
// payload turns out to be 14 MiB or 14 GiB.
func writeMdat(out *os.File, vr, ar io.ReadSeeker, vFrags, aFrags []fragSamples) (vChunkOffsets, aChunkOffsets []uint64, mdatEnd uint64, err error) {
	mdatHeaderStart, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, nil, 0, err
	}
	// Reserve 16 bytes for the largesize mdat header: size=1 (uint32), type
	// 'mdat' (4 bytes), largesize (uint64). We back-patch largesize once the
	// payload is fully streamed in and we know the total length.
	if _, err := out.Write(make([]byte, 16)); err != nil {
		return nil, nil, 0, err
	}

	n := len(vFrags)
	if len(aFrags) > n {
		n = len(aFrags)
	}
	vChunkOffsets = make([]uint64, 0, len(vFrags))
	aChunkOffsets = make([]uint64, 0, len(aFrags))
	for i := 0; i < n; i++ {
		if i < len(vFrags) {
			chunkOff, err := writeChunk(out, vr, vFrags[i].samples)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("video fragment %d: %w", i, err)
			}
			vChunkOffsets = append(vChunkOffsets, chunkOff)
		}
		if i < len(aFrags) {
			chunkOff, err := writeChunk(out, ar, aFrags[i].samples)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("audio fragment %d: %w", i, err)
			}
			aChunkOffsets = append(aChunkOffsets, chunkOff)
		}
	}

	payloadEnd, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, nil, 0, err
	}
	totalSize := uint64(payloadEnd - mdatHeaderStart)

	// Back-patch the 16-byte large-size mdat header.
	hdr := make([]byte, 16)
	binary.BigEndian.PutUint32(hdr[0:4], 1)             // size=1 → use largesize
	copy(hdr[4:8], []byte{'m', 'd', 'a', 't'})
	binary.BigEndian.PutUint64(hdr[8:16], totalSize)
	if _, err := out.WriteAt(hdr, mdatHeaderStart); err != nil {
		return nil, nil, 0, fmt.Errorf("patch mdat header: %w", err)
	}
	if _, err := out.Seek(payloadEnd, io.SeekStart); err != nil {
		return nil, nil, 0, err
	}
	return vChunkOffsets, aChunkOffsets, uint64(payloadEnd), nil
}

// writeChunk streams every sample's bytes from src into out and returns the
// absolute byte position where the chunk started — the value stco/co64 needs.
func writeChunk(out *os.File, src io.ReadSeeker, samples []sampleInfo) (uint64, error) {
	chunkOff, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	for _, s := range samples {
		if _, err := src.Seek(int64(s.srcOffset), io.SeekStart); err != nil {
			return 0, err
		}
		if _, err := io.CopyN(out, src, int64(s.size)); err != nil {
			return 0, err
		}
	}
	return uint64(chunkOff), nil
}

// writeProgressiveMoov writes the moov box at the current output position,
// holding both tracks with full progressive sample tables. The chunk offsets
// recorded by writeMdat populate each track's stco/co64.
func writeProgressiveMoov(out *os.File, vr, ar io.ReadSeeker,
	vInfo, aInfo *trackInfo,
	vFrags, aFrags []fragSamples,
	vChunkOffsets, aChunkOffsets []uint64) error {

	movieTS := vInfo.movieTimescale
	if movieTS == 0 {
		movieTS = 1000
	}
	vDur := totalDuration(vFrags)
	aDur := totalDuration(aFrags)
	vDurMovie := scaleDuration(vDur, vInfo.mediaTimescale, movieTS)
	aDurMovie := scaleDuration(aDur, aInfo.mediaTimescale, movieTS)
	movieDur := vDurMovie
	if aDurMovie > movieDur {
		movieDur = aDurMovie
	}

	w := mp4.NewWriter(out)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}

	// mvhd
	mvhd := &mp4.Mvhd{
		Timescale:   movieTS,
		DurationV0:  uint32(movieDur),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		NextTrackID: 3,
	}
	if err := writeBox(w, mvhd); err != nil {
		return err
	}

	if err := writeTrak(w, vr, vInfo, vFrags, vChunkOffsets, vDurMovie, vDur); err != nil {
		return fmt.Errorf("video trak: %w", err)
	}
	if err := writeTrak(w, ar, aInfo, aFrags, aChunkOffsets, aDurMovie, aDur); err != nil {
		return fmt.Errorf("audio trak: %w", err)
	}

	_, err := w.EndBox()
	return err
}

// totalDuration sums every sample's duration across a track's fragments.
func totalDuration(frags []fragSamples) uint64 {
	var d uint64
	for _, f := range frags {
		for _, s := range f.samples {
			d += uint64(s.duration)
		}
	}
	return d
}

func scaleDuration(d uint64, srcTS, dstTS uint32) uint64 {
	if srcTS == 0 || dstTS == 0 || srcTS == dstTS {
		return d
	}
	return d * uint64(dstTS) / uint64(srcTS)
}

// writeTrak emits one trak: tkhd / mdia(mdhd / hdlr-copied / minf(mhd-copied /
// dinf-copied / stbl(stsd-copied + synthesized stts/stsc/stsz/stss/[ctts]/co64))).
func writeTrak(w *mp4.Writer, src io.ReadSeeker, info *trackInfo,
	frags []fragSamples, chunkOffsets []uint64, dur, mediaDur uint64) error {

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}

	// tkhd: enabled+in-movie+in-preview flags, duration in movie timescale.
	// Width/Height in 16.16 fixed-point come from the source's tkhd — a video
	// track with width=0/height=0 makes Chrome render the <video> element as
	// a zero-pixel box even though the avc1 sample entry has the correct
	// frame size. Audio tracks legitimately carry 0/0 here, which the source
	// already does, so the verbatim copy is correct for both handlers.
	tkhd := &mp4.Tkhd{
		TrackID:    info.trackID,
		DurationV0: uint32(dur),
		Volume:     0x0100,
		Matrix:     [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		Width:      info.tkhdWidth,
		Height:     info.tkhdHeight,
	}
	tkhd.SetFlags(0x000007) // track_enabled | track_in_movie | track_in_preview
	if err := writeBox(w, tkhd); err != nil {
		return err
	}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}

	// mdhd: media timescale and duration.
	mdhd := &mp4.Mdhd{
		Timescale:  info.mediaTimescale,
		DurationV0: uint32(mediaDur),
		Language:   info.mediaLanguage,
		PreDefined: 0,
	}
	if err := writeBox(w, mdhd); err != nil {
		return err
	}

	// hdlr — copy verbatim so the handler name string and reserved fields
	// match the source byte-for-byte.
	if info.hdlrBox != nil {
		if err := w.CopyBox(src, info.hdlrBox); err != nil {
			return err
		}
	}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}

	// vmhd / smhd — copy from source verbatim.
	if info.mhdBox != nil {
		if err := w.CopyBox(src, info.mhdBox); err != nil {
			return err
		}
	}
	// dinf — copy from source verbatim.
	if info.dinfBox != nil {
		if err := w.CopyBox(src, info.dinfBox); err != nil {
			return err
		}
	}

	// stbl — synthesize except for stsd (copied verbatim: it carries the
	// codec sample entries the decoder needs).
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	if info.stsdBox != nil {
		if err := w.CopyBox(src, info.stsdBox); err != nil {
			return err
		}
	}
	if err := writeStts(w, frags); err != nil {
		return err
	}
	hasCtts, allNonNegative := analyzeCtts(frags)
	if hasCtts {
		if err := writeCtts(w, frags, allNonNegative); err != nil {
			return err
		}
	}
	if err := writeStss(w, frags); err != nil {
		return err
	}
	if err := writeStsc(w, frags); err != nil {
		return err
	}
	if err := writeStsz(w, frags); err != nil {
		return err
	}
	if err := writeCo64(w, chunkOffsets); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // minf
		return err
	}
	if _, err := w.EndBox(); err != nil { // mdia
		return err
	}
	_, err := w.EndBox() // trak
	return err
}

// writeStts emits a run-length-encoded decoding-time-to-sample table: every
// run of consecutive samples with the same duration collapses into a single
// (count, delta) entry.
func writeStts(w *mp4.Writer, frags []fragSamples) error {
	var entries []mp4.SttsEntry
	var curDur uint32
	var curCount uint32
	for _, f := range frags {
		for _, s := range f.samples {
			if curCount == 0 {
				curDur = s.duration
				curCount = 1
				continue
			}
			if s.duration == curDur {
				curCount++
				continue
			}
			entries = append(entries, mp4.SttsEntry{SampleCount: curCount, SampleDelta: curDur})
			curDur = s.duration
			curCount = 1
		}
	}
	if curCount > 0 {
		entries = append(entries, mp4.SttsEntry{SampleCount: curCount, SampleDelta: curDur})
	}
	return writeBox(w, &mp4.Stts{EntryCount: uint32(len(entries)), Entries: entries})
}

// analyzeCtts reports whether any sample carries a non-zero composition time
// offset (in which case the trak needs a ctts box) and whether every offset
// fits in the non-negative version-0 form.
func analyzeCtts(frags []fragSamples) (has, allNonNegative bool) {
	allNonNegative = true
	for _, f := range frags {
		for _, s := range f.samples {
			if s.cto != 0 {
				has = true
			}
			if s.cto < 0 {
				allNonNegative = false
			}
		}
	}
	return has, allNonNegative
}

// writeCtts emits a run-length composition-time-offset table — required when
// the decoder must reorder samples for presentation (e.g. AVC with B-frames).
// Version 1 of the box carries signed offsets; version 0 carries unsigned.
func writeCtts(w *mp4.Writer, frags []fragSamples, allNonNegative bool) error {
	type cttsRunEntry struct {
		count  uint32
		offset int32
	}
	var runs []cttsRunEntry
	var curCount uint32
	var curOff int32
	first := true
	for _, f := range frags {
		for _, s := range f.samples {
			if first {
				curOff = s.cto
				curCount = 1
				first = false
				continue
			}
			if s.cto == curOff {
				curCount++
				continue
			}
			runs = append(runs, cttsRunEntry{curCount, curOff})
			curOff = s.cto
			curCount = 1
		}
	}
	if curCount > 0 {
		runs = append(runs, cttsRunEntry{curCount, curOff})
	}

	entries := make([]mp4.CttsEntry, 0, len(runs))
	for _, r := range runs {
		e := mp4.CttsEntry{SampleCount: r.count}
		if allNonNegative {
			e.SampleOffsetV0 = uint32(r.offset)
		} else {
			e.SampleOffsetV1 = r.offset
		}
		entries = append(entries, e)
	}
	ctts := &mp4.Ctts{EntryCount: uint32(len(entries)), Entries: entries}
	if !allNonNegative {
		ctts.SetVersion(1)
	}
	return writeBox(w, ctts)
}

// writeStss emits the sync-sample table. Only meaningful for video tracks; if
// every sample is a sync sample (typical for audio) the box is omitted —
// players treat its absence as "every sample syncs."
func writeStss(w *mp4.Writer, frags []fragSamples) error {
	var syncs []uint32
	var allSync = true
	sampleNum := uint32(0)
	for _, f := range frags {
		for _, s := range f.samples {
			sampleNum++
			if s.isSync {
				syncs = append(syncs, sampleNum)
			} else {
				allSync = false
			}
		}
	}
	if allSync {
		return nil
	}
	return writeBox(w, &mp4.Stss{EntryCount: uint32(len(syncs)), SampleNumber: syncs})
}

// writeStsc emits the sample-to-chunk table. One source fragment = one chunk
// in the output, so each chunk's sample count may vary. The encoding runs
// chunks with the same sample-count into a single entry.
func writeStsc(w *mp4.Writer, frags []fragSamples) error {
	var entries []mp4.StscEntry
	var prevCount uint32
	for i, f := range frags {
		c := uint32(len(f.samples))
		if i == 0 || c != prevCount {
			entries = append(entries, mp4.StscEntry{
				FirstChunk:             uint32(i + 1),
				SamplesPerChunk:        c,
				SampleDescriptionIndex: 1,
			})
			prevCount = c
		}
	}
	return writeBox(w, &mp4.Stsc{EntryCount: uint32(len(entries)), Entries: entries})
}

// writeStsz emits per-sample sizes. The "all samples same size" optimization
// (SampleSize != 0, no per-entry list) is skipped — Reddit's encodes show
// varying sample sizes for both AVC and AAC, so the list form is what's
// actually correct.
func writeStsz(w *mp4.Writer, frags []fragSamples) error {
	var sizes []uint32
	for _, f := range frags {
		for _, s := range f.samples {
			sizes = append(sizes, s.size)
		}
	}
	return writeBox(w, &mp4.Stsz{
		SampleSize:  0,
		SampleCount: uint32(len(sizes)),
		EntrySize:   sizes,
	})
}

// writeCo64 emits chunk offsets as 64-bit values — the output mdat header
// alone uses a 16-byte (large-size) form and the payload can easily exceed
// 4 GiB on long videos, so the 32-bit stco would overflow.
func writeCo64(w *mp4.Writer, chunkOffsets []uint64) error {
	return writeBox(w, &mp4.Co64{
		EntryCount:  uint32(len(chunkOffsets)),
		ChunkOffset: chunkOffsets,
	})
}
