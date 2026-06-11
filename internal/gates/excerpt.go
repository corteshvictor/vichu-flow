package gates

import (
	"fmt"
	"os"
)

// Excerpt returns a bounded slice of a gate's output suitable for handing to an
// agent, honoring the context budget (never pass full logs to an agent). When
// the file exceeds maxBytes it returns the trailing maxBytes — where failures
// usually surface — prefixed with a truncation notice, and truncated=true.
func Excerpt(path string, maxBytes int) (text string, truncated bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false, err
	}
	size := info.Size()
	if size <= int64(maxBytes) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false, err
		}
		return string(data), false, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	buf := make([]byte, maxBytes)
	if _, err := f.ReadAt(buf, size-int64(maxBytes)); err != nil {
		return "", false, err
	}
	notice := fmt.Sprintf("[... %d bytes truncated; full output at %s — showing last %d bytes ...]\n", size-int64(maxBytes), path, maxBytes)
	return notice + string(buf), true, nil
}
