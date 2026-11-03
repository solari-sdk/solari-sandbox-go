package solari

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	// fileChunkSize is how much one fs.upload / fs.download call moves. A bigger frame (a 48 MiB file is a 64 MiB frame once
	// base64'd) closes the control channel and fails every call in flight on it.
	fileChunkSize = 4 << 20
	// maxSingleFrameBytes is the most Write will send in one frame; Upload has no such limit.
	maxSingleFrameBytes = 32 << 20
)

func fileTooLarge(n int) error {
	return newSolariError(fmt.Sprintf("file too large for a single call (%d bytes, max %d): use files.upload()/files.download() or uploadUrl()", n, maxSingleFrameBytes))
}

// isUnknownMethod reports whether a control-channel reply says the guest does not know the method (an older image).
func isUnknownMethod(err error) bool {
	var ae *ActionError
	if errors.As(err, &ae) {
		m := strings.ToLower(ae.Message + " " + ae.Code)
		return strings.Contains(m, "unknown method") || strings.Contains(m, "method not found") || strings.Contains(m, "unknown_method")
	}
	return false
}

// Files is the filesystem namespace on a Sandbox (fs.* RPCs).
type Files struct {
	sb *Sandbox
}

// Read returns the raw bytes of an in-guest file. It reads in 4 MiB chunks (fs.download), so a large file never
// travels as one frame; a guest image that predates fs.download is read in one frame (fs.read).
func (f *Files) Read(ctx context.Context, path string) ([]byte, error) {
	b, err := f.Download(ctx, path)
	if err != nil && isUnknownMethod(err) {
		return f.readSingleFrame(ctx, path)
	}
	return b, err
}

func (f *Files) readSingleFrame(ctx context.Context, path string) ([]byte, error) {
	raw, err := f.sb.ch.call(ctx, "fs.read", map[string]interface{}{"path": path}, callOptions{})
	if err != nil {
		return nil, err
	}
	var res struct {
		Base64 string `json:"base64"`
	}
	if err := unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Base64)
}

// Download reads a whole in-guest file in 4 MiB chunks (fs.download) until the guest reports the end.
func (f *Files) Download(ctx context.Context, path string) ([]byte, error) {
	var out []byte
	offset := int64(0)
	for {
		raw, err := f.sb.ch.call(ctx, "fs.download", map[string]interface{}{
			"path": path, "offset": offset, "length": fileChunkSize,
		}, callOptions{})
		if err != nil {
			return nil, err
		}
		var res struct {
			Base64     string `json:"base64"`
			EOF        bool   `json:"eof"`
			NextOffset *int64 `json:"nextOffset"`
		}
		if err := unmarshal(raw, &res); err != nil {
			return nil, err
		}
		chunk, err := base64.StdEncoding.DecodeString(res.Base64)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if res.EOF {
			return out, nil
		}
		next := offset + int64(len(chunk))
		if res.NextOffset != nil {
			next = *res.NextOffset
		}
		if next <= offset {
			return nil, newSolariError(fmt.Sprintf("Solari: fs.download made no progress at offset %d", offset))
		}
		offset = next
	}
}

// Upload writes data to an in-guest path in 4 MiB chunks (fs.upload): the first call truncates, later calls append at
// their offset. An empty file is one zero-length call. Unlike Write there is no size limit.
func (f *Files) Upload(ctx context.Context, path string, data []byte) error {
	offset := 0
	for {
		end := offset + fileChunkSize
		if end > len(data) {
			end = len(data)
		}
		_, err := f.sb.ch.call(ctx, "fs.upload", map[string]interface{}{
			"path":     path,
			"base64":   base64.StdEncoding.EncodeToString(data[offset:end]),
			"offset":   offset,
			"truncate": offset == 0,
		}, callOptions{})
		if err != nil {
			return err
		}
		if end >= len(data) {
			return nil
		}
		offset = end
	}
}

// ReadText is Read decoded as UTF-8 text.
func (f *Files) ReadText(ctx context.Context, path string) (string, error) {
	b, err := f.Read(ctx, path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Write writes bytes to an in-guest path. mode is the unix permission bits (0
// means "unset" — omitted from the wire). The data travels as one frame, so more than 32 MiB is refused before anything
// is sent (a frame that large would close the control channel and fail every call on it): use Upload.
func (f *Files) Write(ctx context.Context, path string, data []byte, mode int) error {
	if len(data) > maxSingleFrameBytes {
		return fileTooLarge(len(data))
	}
	params := map[string]interface{}{
		"path":   path,
		"base64": base64.StdEncoding.EncodeToString(data),
	}
	if mode != 0 {
		params["mode"] = mode
	}
	_, err := f.sb.ch.call(ctx, "fs.write", params, callOptions{})
	return err
}

// List returns the directory entries at path.
func (f *Files) List(ctx context.Context, path string) ([]FsEntry, error) {
	raw, err := f.sb.ch.call(ctx, "fs.list", map[string]interface{}{"path": path}, callOptions{})
	if err != nil {
		return nil, err
	}
	var res struct {
		Entries []FsEntry `json:"entries"`
	}
	if err := unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Entries, nil
}

// Stat returns metadata for a single path.
func (f *Files) Stat(ctx context.Context, path string) (*FsStat, error) {
	raw, err := f.sb.ch.call(ctx, "fs.stat", map[string]interface{}{"path": path}, callOptions{})
	if err != nil {
		return nil, err
	}
	var res FsStat
	if err := unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Mkdir creates a directory (and parents).
func (f *Files) Mkdir(ctx context.Context, path string) error {
	_, err := f.sb.ch.call(ctx, "fs.mkdir", map[string]interface{}{"path": path}, callOptions{})
	return err
}

// Remove deletes a path; recursive removes a directory tree.
func (f *Files) Remove(ctx context.Context, path string, recursive bool) error {
	_, err := f.sb.ch.call(ctx, "fs.remove", map[string]interface{}{
		"path": path, "recursive": recursive,
	}, callOptions{})
	return err
}

// Rename moves/renames a path.
func (f *Files) Rename(ctx context.Context, from, to string) error {
	_, err := f.sb.ch.call(ctx, "fs.rename", map[string]interface{}{"from": from, "to": to}, callOptions{})
	return err
}
