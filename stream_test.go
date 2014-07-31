package http2

import (
	"bytes"
	"io"
	"reflect"
	"testing"
)

var (
	okHeader = HeaderFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders,
			Length:   0,
			StreamID: 1,
		},
	}
	okFirstHeader = HeaderFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    0,
			Length:   0,
			StreamID: 1,
		},
	}
	okContinuation = ContinuationFrame{
		FrameHeader: FrameHeader{
			Type:     FrameContinuation,
			Flags:    0,
			Length:   0,
			StreamID: 1,
		},
	}
	okLastContinuation = ContinuationFrame{
		FrameHeader: FrameHeader{
			Type:     FrameContinuation,
			Flags:    FlagHeadersEndHeaders,
			Length:   0,
			StreamID: 1,
		},
	}
	okSettings = SettingsFrame{
		FrameHeader: FrameHeader{
			Type:     FrameSettings,
			Flags:    0,
			Length:   0,
			StreamID: 0,
		},
	}
	okWindowUpdate = WindowUpdateFrame{
		FrameHeader: FrameHeader{
			Type:     FrameWindowUpdate,
			Flags:    0,
			Length:   0,
			StreamID: 1,
		},
	}
)

type streamTest struct {
	frames   []Frame
	errIndex int
	err      error
}

func TestStreamExpectContinuation(t *testing.T) {
	// TODO(jmhodges): test timeouts for:
	// newStreamTest(nil, -1, okFirstHeader)
	// newStreamTest(nil, -1, okFirstHeader, okContinuation)

	tests := []streamTest{
		newStreamTest(nil, -1, okHeader),
		newStreamTest(ErrUnexpectedContinuation, 1, okHeader, okLastContinuation),
		newStreamTest(nil, -1, okFirstHeader, okLastContinuation),
		newStreamTest(nil, -1, okFirstHeader, okContinuation, okLastContinuation),
		newStreamTest(ErrUnexpectedContinuation, 2, okFirstHeader, okLastContinuation, okLastContinuation),
		newStreamTest(ErrExpectedContinuation, 1, okFirstHeader, okSettings),
		newStreamTest(ErrExpectedContinuation, 2, okFirstHeader, okContinuation, okSettings),
		newStreamTest(ErrExpectedContinuation, 1, okFirstHeader, okWindowUpdate),
		newStreamTest(ErrExpectedContinuation, 2, okFirstHeader, okContinuation, okWindowUpdate),
		newStreamTest(ErrUnexpectedContinuation, 3, okFirstHeader, okContinuation, okLastContinuation, okLastContinuation),
		// TODO(jmhodges): newStreamTest(ErrStreamAlreadyEnded, 1, okHeaderLastInStream, okHeader),
		// TODO(jmhodges): test for each frame type after okFirstHeader
	}
	for i, tt := range tests {
		s := &streamHandler{}
		var err error
		errIndex := -1
		for j, f := range tt.frames {
			err = s.Handle(f, emptyReader())
			if err != nil {
				errIndex = j
				break
			}
		}
		if !reflect.DeepEqual(tt.err, err) {
			t.Errorf("#%d, error: want %#v, got %#v", i, tt.err, err)
		}
		if tt.errIndex != errIndex {
			t.Errorf("#%d, errorIndex: want %#v, got %#v", i, tt.errIndex, errIndex)
		}

	}

}

func emptyReader() *io.LimitedReader {
	return &io.LimitedReader{R: &bytes.Buffer{}}
}

func newStreamTest(err error, errIndex int, fs ...Frame) streamTest {
	if len(fs) == 0 {
		panic("Not able to make a frame test with no frames.")
	}
	return streamTest{
		frames:   fs,
		err:      err,
		errIndex: errIndex,
	}
}
