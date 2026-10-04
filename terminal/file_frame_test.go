package terminal

import (
	"bytes"
	"testing"
)

func TestUploadFrameRoundTrip(t *testing.T) {
	frame, err := encodeUploadFrame("ab", "cd", 1, []byte{0x09})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		'L', 'T', 'F', '1', 1,
		0, 2, 'a', 'b',
		0, 2, 'c', 'd',
		0, 0, 0, 0, 0, 0, 0, 1,
		0x09,
	}
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = %x, want %x", frame, want)
	}
	parsed, ok := parseUploadFrame(frame)
	if !ok || parsed.requestID != "ab" || parsed.uploadID != "cd" || parsed.offset != 1 || !bytes.Equal(parsed.payload, []byte{0x09}) {
		t.Fatalf("parsed = %#v, ok %v", parsed, ok)
	}
	if _, ok := parseDownloadFrame(frame); ok {
		t.Fatal("upload frame parsed as download")
	}
}

func TestDownloadFrameRoundTrip(t *testing.T) {
	frame, err := encodeDownloadFrame("ab", 7, []byte{0x09})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		'L', 'T', 'F', '1', 2,
		0, 2, 'a', 'b',
		0, 0, 0, 0, 0, 0, 0, 7,
		0x09,
	}
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = %x, want %x", frame, want)
	}
	parsed, ok := parseDownloadFrame(frame)
	if !ok || parsed.requestID != "ab" || parsed.sequence != 7 || !bytes.Equal(parsed.payload, []byte{0x09}) {
		t.Fatalf("parsed = %#v, ok %v", parsed, ok)
	}
}

func TestMalformedMagicIsLeftForTheTerminal(t *testing.T) {
	manager := newFileManager(&recordingFileWriter{})
	t.Cleanup(manager.close)
	if manager.enqueueFileFrame([]byte{'L', 'T', 'F', '1', 0xff}) {
		t.Fatal("malformed frame was consumed")
	}
	frame, err := encodeUploadFrame("ab", "cd", 1, []byte{0x09})
	if err != nil {
		t.Fatal(err)
	}
	if !manager.enqueueFileFrame(frame) {
		t.Fatal("valid upload frame was not consumed")
	}
}

func TestTerminalBytesAreNotFileFrames(t *testing.T) {
	if isFileFrame([]byte("ls\r")) || isFileFrame([]byte{'L', 'T', 'F'}) {
		t.Fatal("terminal bytes were treated as a file frame")
	}
	if !isFileFrame([]byte{'L', 'T', 'F', '1', 1}) {
		t.Fatal("file frame magic was not recognized")
	}
}
