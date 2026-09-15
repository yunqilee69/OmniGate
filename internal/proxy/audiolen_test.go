package proxy

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestWAVDuration(t *testing.T) {
	// 8kHz, mono, 16bit, 800 samples = 0.1s
	dataSize := uint32(1600)
	buf := make([]byte, 44+int(dataSize))
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 36+dataSize)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:28], 8000)
	binary.LittleEndian.PutUint32(buf[28:32], 16000)
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataSize)

	sec, ok := audioDuration("a.wav", buf)
	if !ok {
		t.Fatal("wav duration not parsed")
	}
	if math.Abs(sec-0.1) > 1e-9 {
		t.Fatalf("wav duration = %v, want 0.1", sec)
	}
}

func TestFLACDuration(t *testing.T) {
	// STREAMINFO: sample_rate=8000, total_samples=800 → 0.1s
	buf := make([]byte, 4+4+34)
	copy(buf[0:4], "fLaC")
	buf[4] = 0x80 // last block, type 0
	buf[5], buf[6], buf[7] = 0, 0, 34
	// bytes 10-12 of STREAMINFO: sampleRate 20bit starting at offset 10
	// sampleRate 8000 = 0x1F40 → packed at b[10..12]
	b := buf[8:]
	b[10] = 0x1F
	b[11] = 0x40
	b[12] = 0x00 // high 4 of sampleRate already in b[11] low? 8000 = 0001 1111 0100 0000
	// 20-bit sampleRate occupies b[10] (8) | b[11] (8) | b[12] high 4
	// 8000 = 0x01F40 → 0000 0001 1111 0100 0000
	b[10] = 0x01
	b[11] = 0xF4
	b[12] = 0x00
	// total_samples 36bit in b[13] low 4 + b[14..17]
	total := uint64(800)
	b[13] = byte((total >> 32) & 0x0f)
	b[14] = byte(total >> 24)
	b[15] = byte(total >> 16)
	b[16] = byte(total >> 8)
	b[17] = byte(total)

	sec, ok := audioDuration("a.flac", buf)
	if !ok {
		t.Fatal("flac duration not parsed")
	}
	if math.Abs(sec-0.1) > 1e-9 {
		t.Fatalf("flac duration = %v, want 0.1", sec)
	}
}

func TestMP4Duration(t *testing.T) {
	// ftyp + moov/mvhd version 0, timescale=1000, duration=2500 → 2.5s
	ftyp := make([]byte, 16)
	binary.BigEndian.PutUint32(ftyp[0:4], 16)
	copy(ftyp[4:8], "ftyp")
	copy(ftyp[8:12], "isom")

	mvhdPayload := make([]byte, 20) // version(1)+flags(3)+ctime(4)+mtime(4)+timescale(4)+duration(4)
	binary.BigEndian.PutUint32(mvhdPayload[12:16], 1000)
	binary.BigEndian.PutUint32(mvhdPayload[16:20], 2500)
	mvhd := make([]byte, 8+len(mvhdPayload))
	binary.BigEndian.PutUint32(mvhd[0:4], uint32(len(mvhd)))
	copy(mvhd[4:8], "mvhd")
	copy(mvhd[8:], mvhdPayload)

	moov := make([]byte, 8+len(mvhd))
	binary.BigEndian.PutUint32(moov[0:4], uint32(len(moov)))
	copy(moov[4:8], "moov")
	copy(moov[8:], mvhd)

	buf := append(ftyp, moov...)
	sec, ok := audioDuration("a.m4a", buf)
	if !ok {
		t.Fatal("m4a duration not parsed")
	}
	if math.Abs(sec-2.5) > 1e-9 {
		t.Fatalf("m4a duration = %v, want 2.5", sec)
	}
}

func TestOggOpusDuration(t *testing.T) {
	// 单页，granulepos=4800 → 0.1s at 48kHz
	nseg := 1
	page := make([]byte, 27+nseg+8)
	copy(page[0:4], "OggS")
	page[4] = 0
	binary.LittleEndian.PutUint64(page[6:14], 4800)
	page[26] = byte(nseg)
	page[27] = 8
	copy(page[28:], "OpusHead")
	sec, ok := audioDuration("a.ogg", page)
	if !ok {
		t.Fatal("ogg duration not parsed")
	}
	if math.Abs(sec-0.1) > 1e-9 {
		t.Fatalf("ogg duration = %v, want 0.1", sec)
	}
}

func TestMP3CBRDuration(t *testing.T) {
	// MPEG-1 Layer III, 128kbps, 44100Hz, no pad → frame size 417
	// 10 frames × 1152 / 44100 ≈ 0.261s
	const nFrames = 10
	hdr := uint32(0xFFFB9000) // sync+mpeg1+l3+no crc + br 128 + sr 44100
	frameSize := 417
	buf := make([]byte, nFrames*frameSize)
	for i := 0; i < nFrames; i++ {
		binary.BigEndian.PutUint32(buf[i*frameSize:], hdr)
	}
	sec, ok := audioDuration("a.mp3", buf)
	if !ok {
		t.Fatal("mp3 duration not parsed")
	}
	want := float64(nFrames) * 1152 / 44100
	if math.Abs(sec-want) > 1e-6 {
		t.Fatalf("mp3 duration = %v, want %v", sec, want)
	}
}

func TestAudioDurationRejectsGarbage(t *testing.T) {
	tests := [][]byte{
		nil,
		[]byte("short"),
		[]byte("RIFF....NOTWAVE"),
		{0xff, 0xff, 0xff, 0xff},
	}
	for i, in := range tests {
		if sec, ok := audioDuration("x", in); ok || sec != 0 {
			t.Errorf("case %d: got (%v,%v), want (0,false)", i, sec, ok)
		}
	}
}
