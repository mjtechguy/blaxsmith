// Package limit bounds output retained from subprocesses.
package limit

import (
	"bytes"
	"fmt"
)

type Buffer struct {
	Max int
	buf bytes.Buffer
}

func (b *Buffer) String() string { return b.buf.String() }
func (b *Buffer) Bytes() []byte  { return b.buf.Bytes() }

func (b *Buffer) Write(p []byte) (int, error) {
	if len(p) > b.Max-b.buf.Len() {
		return 0, fmt.Errorf("subprocess output exceeds %d bytes", b.Max)
	}
	return b.buf.Write(p)
}
