package http2

import (
	"errors"
	"io"
	"io/ioutil"
	"log"
)

var (
	ErrExpectedContinuation   = errors.New("http2: expected a Continuation frame to come next.")
	ErrUnexpectedContinuation = errors.New("http2: no Continuation frame expected, but one arrived.")
)

type streamHandler struct {
	// No interleaving of Header and Continuation frames from
	// different stream ids are allowed, so all we need is a bool, not
	// a map[streamID]bool.
	expectContinuation bool
	// TODO(jmhodges): request timeouts
}

func (s *streamHandler) Handle(f Frame, frameReader *io.LimitedReader) error {
	// TODO: remove debug lines here.
	// log.Printf("got frame: %#v", f)
	if s.expectContinuation && f.Header().Type != FrameContinuation {
		return ErrExpectedContinuation
	}
	switch f := f.(type) {
	case HeaderFrame:
		return s.handleHeaderFrame(f, frameReader)
	case ContinuationFrame:
		if !s.expectContinuation {
			return ErrUnexpectedContinuation
		}
		return s.handleContinuationFrame(f, frameReader)
	default:
		log.Printf("don't know how to handle that one, yet")
		if n, err := io.Copy(ioutil.Discard, frameReader); n > 0 {
			log.Printf("Frame reader for %s failed to read %d bytes", f.Header().Type, n)
			return err
		}

	}
	return nil
}

func (s *streamHandler) handleHeaderFrame(f HeaderFrame, frameReader *io.LimitedReader) error {
	s.expectContinuation = f.isContinued()
	if n, _ := io.Copy(ioutil.Discard, frameReader); n > 0 {
		log.Printf("Frame reader for %s failed to read %d bytes", f.FrameHeader.Type, n)
		return nil
	}
	return nil
}

func (s *streamHandler) handleContinuationFrame(f ContinuationFrame, frameReader *io.LimitedReader) error {
	s.expectContinuation = f.isContinued()
	if n, _ := io.Copy(ioutil.Discard, frameReader); n > 0 {
		log.Printf("Frame reader for %s failed to read %d bytes", f.FrameHeader.Type, n)
		return nil
	}
	return nil
}
