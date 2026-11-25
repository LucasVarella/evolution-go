package send_service

import (
	"encoding/base64"
	"os/exec"
	"testing"
)

func TestFetchStickerInputBase64(t *testing.T) {
	want := []byte("RIFF\x00\x00\x00\x00WEBP")
	enc := base64.StdEncoding.EncodeToString(want)
	for _, in := range []string{enc, "data:image/webp;base64," + enc} {
		got, err := fetchStickerInput(in)
		if err != nil || string(got) != string(want) {
			t.Fatalf("fetchStickerInput(%q) = %q, %v", in, got, err)
		}
	}
}

func ffmpegOrSkip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

func TestInspectWebPAnimated(t *testing.T) {
	ffmpegOrSkip(t)
	out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=512x512:rate=10:duration=1",
		"-c:v", "libwebp_anim", "-loop", "0", "-f", "webp", "pipe:1").Output()
	if err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}
	w, h, animated := inspectWebP(out)
	if w != 512 || h != 512 || !animated {
		t.Fatalf("inspectWebP = %dx%d animated=%v", w, h, animated)
	}
	data, err := stickerWebP(base64.StdEncoding.EncodeToString(out))
	if err != nil || len(data) != len(out) {
		t.Fatalf("stickerWebP should pass WebP through unchanged: len=%d err=%v", len(data), err)
	}
}

func TestConvertAudioToStickerOpus(t *testing.T) {
	ffmpegOrSkip(t)
	wav, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=2.3",
		"-f", "wav", "pipe:1").Output()
	if err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}
	ogg, seconds, err := convertAudioToStickerOpus(wav)
	if err != nil {
		t.Fatal(err)
	}
	if string(ogg[:4]) != "OggS" || len(ogg) > maxStickerAudioBytes {
		t.Fatalf("unexpected output: %d bytes, header %q", len(ogg), ogg[:4])
	}
	if seconds != 3 {
		t.Fatalf("seconds = %d, want 3 (rounded up)", seconds)
	}
}
