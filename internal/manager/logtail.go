package manager

import (
	"io"
	"os"
	"strings"
)

// TailLog 返回日志文件最后 count 行；只读取末尾 512 KiB，文件不存在或不可读时返回 nil。
func TailLog(path string, count int) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	const window = 512 << 10
	offset := int64(0)
	if info.Size() > window {
		offset = info.Size() - window
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(file, window))
	if err != nil {
		return nil
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if offset > 0 && len(lines) > 1 {
		// 从窗口中间开始读时首行可能被截断；只有窗口起点不在行首
		// （前一个字节不是换行）才丢弃。
		boundary := make([]byte, 1)
		if _, err := file.ReadAt(boundary, offset-1); err != nil || boundary[0] != '\n' {
			lines = lines[1:]
		}
	}
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return lines
}
