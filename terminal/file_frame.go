package terminal

import (
	"encoding/binary"
	"errors"
)

var errInvalidFileFrame = errors.New("invalid file frame")

// Binary file frames share the remote WebSocket with terminal bytes.
// A frame starts with "LTF1". Anything else stays terminal input or output.
const (
	fileFrameUpload   = 1
	fileFrameDownload = 2
	fileFrameMaxID    = 64
)

var fileFrameMagic = []byte{'L', 'T', 'F', '1'}

func isFileFrame(payload []byte) bool {
	return len(payload) >= len(fileFrameMagic) && string(payload[:len(fileFrameMagic)]) == string(fileFrameMagic)
}

func encodeUploadFrame(requestID, uploadID string, offset uint64, payload []byte) ([]byte, error) {
	if err := validateFrameID(requestID); err != nil {
		return nil, err
	}
	if err := validateFrameID(uploadID); err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(fileFrameMagic)+1+4+len(requestID)+len(uploadID)+8+len(payload))
	frame = append(frame, fileFrameMagic...)
	frame = append(frame, fileFrameUpload)
	frame = appendFrameID(frame, requestID)
	frame = appendFrameID(frame, uploadID)
	frame = binary.BigEndian.AppendUint64(frame, offset)
	frame = append(frame, payload...)
	return frame, nil
}

func encodeDownloadFrame(requestID string, sequence uint64, payload []byte) ([]byte, error) {
	if err := validateFrameID(requestID); err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(fileFrameMagic)+1+2+len(requestID)+8+len(payload))
	frame = append(frame, fileFrameMagic...)
	frame = append(frame, fileFrameDownload)
	frame = appendFrameID(frame, requestID)
	frame = binary.BigEndian.AppendUint64(frame, sequence)
	frame = append(frame, payload...)
	return frame, nil
}

type uploadFrame struct {
	requestID string
	uploadID  string
	offset    uint64
	payload   []byte
}

type downloadFrame struct {
	requestID string
	sequence  uint64
	payload   []byte
}

func parseUploadFrame(payload []byte) (uploadFrame, bool) {
	rest, ok := frameHeader(payload, fileFrameUpload)
	if !ok {
		return uploadFrame{}, false
	}
	requestID, rest, ok := takeFrameID(rest)
	if !ok {
		return uploadFrame{}, false
	}
	uploadID, rest, ok := takeFrameID(rest)
	if !ok || len(rest) < 8 {
		return uploadFrame{}, false
	}
	offset := binary.BigEndian.Uint64(rest[:8])
	return uploadFrame{
		requestID: requestID,
		uploadID:  uploadID,
		offset:    offset,
		payload:   append([]byte(nil), rest[8:]...),
	}, true
}

func parseDownloadFrame(payload []byte) (downloadFrame, bool) {
	rest, ok := frameHeader(payload, fileFrameDownload)
	if !ok {
		return downloadFrame{}, false
	}
	requestID, rest, ok := takeFrameID(rest)
	if !ok || len(rest) < 8 {
		return downloadFrame{}, false
	}
	sequence := binary.BigEndian.Uint64(rest[:8])
	return downloadFrame{
		requestID: requestID,
		sequence:  sequence,
		payload:   append([]byte(nil), rest[8:]...),
	}, true
}

func frameHeader(payload []byte, kind byte) ([]byte, bool) {
	if len(payload) < len(fileFrameMagic)+1 || string(payload[:len(fileFrameMagic)]) != string(fileFrameMagic) {
		return nil, false
	}
	if payload[len(fileFrameMagic)] != kind {
		return nil, false
	}
	return payload[len(fileFrameMagic)+1:], true
}

func appendFrameID(frame []byte, id string) []byte {
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(id)))
	return append(frame, id...)
}

func takeFrameID(payload []byte) (string, []byte, bool) {
	if len(payload) < 2 {
		return "", nil, false
	}
	size := int(binary.BigEndian.Uint16(payload[:2]))
	if size == 0 || size > fileFrameMaxID || len(payload) < 2+size {
		return "", nil, false
	}
	return string(payload[2 : 2+size]), payload[2+size:], true
}

func validateFrameID(id string) error {
	if id == "" || len(id) > fileFrameMaxID {
		return errInvalidFileFrame
	}
	return nil
}
