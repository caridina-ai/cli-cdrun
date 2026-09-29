//go:build windows

package main

import (
	"encoding/json"
	"os"
	"unicode/utf8"
)

const maxTurns = 16
const maxAnswerBytes = 32 * 1024
const maxLogBytes = 8 * 1024 * 1024

func appendAnswer(t *turn, text string) {
	remaining := maxAnswerBytes - len(t.Answer)
	if len(text) > remaining {
		text = text[:remaining]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		t.Truncated = true
	}
	t.Answer += text
}

// Consume all stderr even after reaching the disk limit, preventing pipe stalls.
type cappedWriter struct {
	file    *os.File
	limit   int64
	written int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := w.limit - w.written; remaining > 0 {
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
		count, err := w.file.Write(p)
		w.written += int64(count)
		if err != nil {
			return count, err
		}
	}
	return n, nil
}

func appendEvent(f *os.File, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(b))+1 > maxLogBytes {
		return nil
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
