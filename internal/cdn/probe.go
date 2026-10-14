package cdn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrFileGone means the file was deleted: a DoodStream CDN answers 200 with "error_nofile".
var ErrFileGone = errors.New("file deleted from CDN (error_nofile)")

// headLen is how much of a file Probe reads: enough to check its container.
const headLen = 64

// Probe returns a stream's size and first bytes from a range request (presigned R2 URLs are signed for GET
// only).
func Probe(stream string) (size int64, head []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := get(ctx, stream, fmt.Sprintf("bytes=0-%d", headLen-1))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	// Reading the whole range lets the stream that follows reuse the connection.
	head, err = io.ReadAll(io.LimitReader(resp.Body, headLen))
	if size := RangeSize(resp); size > 0 {
		return size, head, Redact(err)
	}
	if bytes.Contains(head, []byte("error_nofile")) {
		return 0, nil, ErrFileGone
	}
	return 0, nil, fmt.Errorf("size probe %s, Content-Range %q", resp.Status, resp.Header.Get("Content-Range"))
}
