package usage

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// Lines are read at any length, but only up to the limit is ever held.
func TestReadLineBoundsMemory(t *testing.T) {
	input := "short\n" + strings.Repeat("x", 100) + "\n\n" + strings.Repeat("y", 10) + "\ntail"
	reader := bufio.NewReaderSize(strings.NewReader(input), 16)
	type got struct {
		line     string
		isNil    bool
		size     int64
		complete bool
	}
	var lines []got
	var total int64
	for {
		line, size, complete, err := readLine(reader, 20)
		if size > 0 {
			lines = append(lines, got{string(line), line == nil, size, complete})
			total += size
		}
		if err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
	}
	want := []got{{"short", false, 6, true}, {"", true, 101, true}, {"", false, 1, true}, {strings.Repeat("y", 10), false, 11, true}, {"tail", false, 4, false}}
	if len(lines) != len(want) {
		t.Fatalf("%+v", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d: %+v, want %+v", i, lines[i], want[i])
		}
	}
	if total != int64(len(input)) {
		t.Fatalf("counted %d of %d bytes", total, len(input))
	}
	// A line exactly at the limit is kept; one byte more is not.
	for n, kept := range map[int]bool{20: true, 21: false} {
		line, _, _, _ := readLine(bufio.NewReaderSize(strings.NewReader(strings.Repeat("z", n)+"\n"), 16), 20)
		if (line != nil) != kept {
			t.Errorf("a %d-byte line: kept=%v", n, line != nil)
		}
	}
	// An endless line never grows what is held.
	endless := io.LimitReader(repeating('a'), 64<<20)
	line, size, complete, _ := readLine(bufio.NewReaderSize(endless, 64<<10), 1<<20)
	if line != nil || size != 64<<20 || complete {
		t.Fatalf("endless line: nil=%v size=%d complete=%v", line == nil, size, complete)
	}
}

type repeating byte

func (r repeating) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}
