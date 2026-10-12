package cdn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrFileGone is a DoodStream CDN's answer for a deleted file.
var ErrFileGone = errors.New("file deleted from CDN (error_nofile)")

// ProbeSize returns a stream's size from a 1-byte range request. Not HEAD: some sources are presigned
// S3/R2 URLs, signed for GET only. A DoodStream CDN whose file is gone answers 200 with "error_nofile".
func ProbeSize(stream string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := get(ctx, stream, "bytes=0-0")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if size := RangeSize(resp); size > 0 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1)) // read the byte so the connection is reused for the stream that follows
		return size, nil
	}
	if b, _ := io.ReadAll(io.LimitReader(resp.Body, 64)); bytes.Contains(b, []byte("error_nofile")) {
		return 0, ErrFileGone
	}
	return 0, fmt.Errorf("size probe %s, Content-Range %q", resp.Status, resp.Header.Get("Content-Range"))
}
