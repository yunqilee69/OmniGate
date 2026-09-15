package proxy

import (
	"bytes"
	"encoding/binary"
)

// audioDuration 尽量从音频容器头解析时长（秒）。
// 覆盖 WAV / MP3 / FLAC / M4A(MP4) / Ogg Opus；未识别或截断返回 (0, false)。
func audioDuration(filename string, data []byte) (float64, bool) {
	if len(data) < 12 {
		return 0, false
	}
	if sec, ok := wavDuration(data); ok {
		return sec, true
	}
	if sec, ok := flacDuration(data); ok {
		return sec, true
	}
	if sec, ok := mp4Duration(data); ok {
		return sec, true
	}
	if sec, ok := oggOpusDuration(data); ok {
		return sec, true
	}
	if sec, ok := mp3Duration(data); ok {
		return sec, true
	}
	_ = filename
	return 0, false
}

func wavDuration(data []byte) (float64, bool) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, false
	}
	off := 12
	var sampleRate, channels, bits uint32
	var dataBytes uint32
	haveFmt, haveData := false, false
	for off+8 <= len(data) {
		id := string(data[off : off+4])
		sz := binary.LittleEndian.Uint32(data[off+4 : off+8])
		body := off + 8
		if body+int(sz) > len(data) && id != "data" {
			return 0, false
		}
		switch id {
		case "fmt ":
			if sz < 16 || body+16 > len(data) {
				return 0, false
			}
			channels = uint32(binary.LittleEndian.Uint16(data[body+2 : body+4]))
			sampleRate = binary.LittleEndian.Uint32(data[body+4 : body+8])
			bits = uint32(binary.LittleEndian.Uint16(data[body+14 : body+16]))
			haveFmt = true
		case "data":
			dataBytes = sz
			haveData = true
		}
		if haveFmt && haveData {
			break
		}
		step := 8 + int(sz)
		if sz%2 == 1 {
			step++
		}
		off += step
	}
	if !haveFmt || !haveData || sampleRate == 0 || channels == 0 || bits == 0 {
		return 0, false
	}
	bytesPerSec := sampleRate * channels * (bits / 8)
	if bytesPerSec == 0 {
		return 0, false
	}
	return float64(dataBytes) / float64(bytesPerSec), true
}

func flacDuration(data []byte) (float64, bool) {
	if len(data) < 42 || string(data[0:4]) != "fLaC" {
		return 0, false
	}
	off := 4
	for off+4 <= len(data) {
		header := data[off]
		size := int(data[off+1])<<16 | int(data[off+2])<<8 | int(data[off+3])
		off += 4
		if off+size > len(data) {
			return 0, false
		}
		blockType := header & 0x7f
		if blockType == 0 { // STREAMINFO
			if size < 18 {
				return 0, false
			}
			b := data[off : off+18]
			sampleRate := uint32(b[10])<<12 | uint32(b[11])<<4 | uint32(b[12])>>4
			totalSamples := uint64(b[13]&0x0f)<<32 | uint64(b[14])<<24 | uint64(b[15])<<16 | uint64(b[16])<<8 | uint64(b[17])
			if sampleRate == 0 || totalSamples == 0 {
				return 0, false
			}
			return float64(totalSamples) / float64(sampleRate), true
		}
		off += size
		if header&0x80 != 0 {
			break
		}
	}
	return 0, false
}

func mp4Duration(data []byte) (float64, bool) {
	if len(data) < 8 {
		return 0, false
	}
	if string(data[4:8]) != "ftyp" && string(data[4:8]) != "moov" {
		if findBox(data, "ftyp") < 0 && findBox(data, "moov") < 0 {
			return 0, false
		}
	}
	moov := findBox(data, "moov")
	if moov < 0 {
		return 0, false
	}
	moovSize := int(binary.BigEndian.Uint32(data[moov : moov+4]))
	if moovSize < 16 || moov+moovSize > len(data) {
		return 0, false
	}
	mvhd := findBox(data[moov+8:moov+moovSize], "mvhd")
	if mvhd < 0 {
		return 0, false
	}
	abs := moov + 8 + mvhd
	if abs+8 > len(data) {
		return 0, false
	}
	boxSize := int(binary.BigEndian.Uint32(data[abs : abs+4]))
	payload := abs + 8
	if payload >= len(data) || boxSize < 20 {
		return 0, false
	}
	version := data[payload]
	if version == 1 {
		if payload+32 > len(data) {
			return 0, false
		}
		timescale := binary.BigEndian.Uint32(data[payload+20 : payload+24])
		duration := binary.BigEndian.Uint64(data[payload+24 : payload+32])
		if timescale == 0 {
			return 0, false
		}
		return float64(duration) / float64(timescale), true
	}
	if payload+20 > len(data) {
		return 0, false
	}
	timescale := binary.BigEndian.Uint32(data[payload+12 : payload+16])
	duration := binary.BigEndian.Uint32(data[payload+16 : payload+20])
	if timescale == 0 {
		return 0, false
	}
	return float64(duration) / float64(timescale), true
}

func findBox(data []byte, typ string) int {
	off := 0
	for off+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		if size == 0 {
			size = len(data) - off
		}
		if size == 1 {
			if off+16 > len(data) {
				return -1
			}
			size = int(binary.BigEndian.Uint64(data[off+8 : off+16]))
		}
		if size < 8 {
			return -1
		}
		if string(data[off+4:off+8]) == typ {
			return off
		}
		off += size
	}
	return -1
}

func oggOpusDuration(data []byte) (float64, bool) {
	if len(data) < 27 || string(data[0:4]) != "OggS" {
		return 0, false
	}
	if !bytes.Contains(data[:min(len(data), 512)], []byte("OpusHead")) {
		return 0, false
	}
	off := 0
	var lastGranule int64
	found := false
	for off+27 <= len(data) {
		if string(data[off:off+4]) != "OggS" {
			break
		}
		nseg := int(data[off+26])
		if off+27+nseg > len(data) {
			break
		}
		lastGranule = int64(binary.LittleEndian.Uint64(data[off+6 : off+14]))
		found = true
		pageSize := 27 + nseg
		for i := 0; i < nseg; i++ {
			pageSize += int(data[off+27+i])
		}
		off += pageSize
	}
	if !found || lastGranule <= 0 {
		return 0, false
	}
	return float64(lastGranule) / 48000, true
}

// MPEG-1 Layer III 帧采样数 1152；MPEG-2/2.5 Layer III 为 576。
var mp3Bitrate = [2][16]int{
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}, // MPEG-1 L3
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},     // MPEG-2/2.5 L3
}

func mp3Duration(data []byte) (float64, bool) {
	off := 0
	if len(data) >= 10 && string(data[0:3]) == "ID3" {
		sz := int(data[6])<<21 | int(data[7])<<14 | int(data[8])<<7 | int(data[9])
		off = 10 + sz
		if off > len(data) {
			return 0, false
		}
	}
	frame := findMP3Frame(data[off:])
	if frame < 0 {
		return 0, false
	}
	abs := off + frame
	if abs+4 > len(data) {
		return 0, false
	}
	hdr := binary.BigEndian.Uint32(data[abs : abs+4])
	verID := (hdr >> 19) & 0x3
	layer := (hdr >> 17) & 0x3
	brIdx := (hdr >> 12) & 0xf
	srIdx := (hdr >> 10) & 0x3
	pad := (hdr >> 9) & 0x1
	if layer != 1 { // Layer III = 01
		return 0, false
	}
	mpeg1 := verID == 3
	var bitrate int
	if mpeg1 {
		bitrate = mp3Bitrate[0][brIdx]
	} else {
		bitrate = mp3Bitrate[1][brIdx]
	}
	var sampleRate int
	switch verID {
	case 3: // MPEG-1
		rates := [4]int{44100, 48000, 32000, 0}
		sampleRate = rates[srIdx]
	case 2: // MPEG-2
		rates := [4]int{22050, 24000, 16000, 0}
		sampleRate = rates[srIdx]
	case 0: // MPEG-2.5
		rates := [4]int{11025, 12000, 8000, 0}
		sampleRate = rates[srIdx]
	default:
		return 0, false
	}
	if bitrate == 0 || sampleRate == 0 {
		return 0, false
	}
	samplesPerFrame := 1152
	if !mpeg1 {
		samplesPerFrame = 576
	}
	frameSize := (samplesPerFrame/8)*bitrate*1000/sampleRate + int(pad)

	// Xing/Info VBR tag 在第一帧 side info 之后
	side := 4
	channelMode := (hdr >> 6) & 0x3
	if mpeg1 {
		if channelMode == 3 {
			side += 17
		} else {
			side += 32
		}
	} else {
		if channelMode == 3 {
			side += 9
		} else {
			side += 17
		}
	}
	tagOff := abs + side
	if tagOff+8 <= len(data) {
		tag := string(data[tagOff : tagOff+4])
		if tag == "Xing" || tag == "Info" {
			flags := binary.BigEndian.Uint32(data[tagOff+4 : tagOff+8])
			if flags&0x1 != 0 && tagOff+12 <= len(data) {
				frames := binary.BigEndian.Uint32(data[tagOff+8 : tagOff+12])
				if frames > 0 {
					return float64(frames) * float64(samplesPerFrame) / float64(sampleRate), true
				}
			}
		}
	}
	if frameSize <= 0 {
		return 0, false
	}
	remain := len(data) - abs
	nFrames := remain / frameSize
	if nFrames <= 0 {
		return 0, false
	}
	return float64(nFrames) * float64(samplesPerFrame) / float64(sampleRate), true
}

func findMP3Frame(data []byte) int {
	for i := 0; i+1 < len(data); i++ {
		if data[i] == 0xff && data[i+1]&0xe0 == 0xe0 {
			return i
		}
	}
	return -1
}
