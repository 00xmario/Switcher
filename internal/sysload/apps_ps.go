package sysload

import (
	"bufio"
	"bytes"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// parsePS reads two ps listings: pid, processor time, resident kilobytes,
// start and the kernel's name for the program (16 characters at most); and
// pid and the path each was started as. That path is used only when it is
// the program's own (absolute, ending in the kernel's name): a process may
// rewrite it with its arguments, as PostgreSQL does.
func parsePS(stats, paths []byte) procSnapshot {
	pathOf := map[int]string{}
	lines(paths, func(f []string) {
		if pid, err := strconv.Atoi(f[0]); err == nil && len(f) > 1 {
			pathOf[pid] = strings.Join(f[1:], " ")
		}
	})
	var snap procSnapshot
	lines(stats, func(f []string) {
		// pid time rss, then the start (Mon Nov 10 18:01:02 2026), then the
		// name, which may hold spaces.
		if len(f) < 9 {
			return
		}
		pid, err1 := strconv.Atoi(f[0])
		cpu, ok := psTime(f[1])
		rss, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || err2 != nil || !ok || pid <= 0 {
			return
		}
		h := fnv.New64a()
		h.Write([]byte(strings.Join(f[3:8], " ")))
		name := strings.Join(f[8:], " ")
		if strings.HasPrefix(name, "<") || name == "" {
			return // a zombie: no memory, no time
		}
		if path := pathOf[pid]; ownPath(path, name) {
			name = appName(path)
		}
		snap.procs = append(snap.procs, procInfo{pid: pid, start: h.Sum64(), name: friendlyName(name), cpu: cpu, mem: rss << 10})
	})
	return snap
}

// isFile says a path is a program file on this computer; tests replace it.
var isFile = cachedIsFile

// Paths not looked at: network and removable volumes, whose server may not
// answer (or which a process could name to make this computer reach one),
// and the folders macOS asks the user before an app may look into.
var unlookedPaths = []string{"/Volumes/", "/net/", "/Network/", "/automount/"}
var unlookedFolders = []string{"/Desktop/", "/Documents/", "/Downloads/", "/Library/Mobile Documents/", "/Library/CloudStorage/"}

// fileCache remembers what was found for a while: the same paths come back
// with every listing. pending holds the paths being looked at right now.
var fileCache = struct {
	sync.Mutex
	found   map[string]bool
	pending map[string]bool
	at      time.Time
}{found: map[string]bool{}, pending: map[string]bool{}}

const fileCacheFor = time.Minute

// statWait is how long a look at one path may take. Any process chooses the
// path it shows, so the look must not be able to hold up the listing.
var statWait = 250 * time.Millisecond

// lookedAt reports whether a path is one this computer may look at.
func lookedAt(path string) bool {
	// Spelled plainly: no ".." or "//" to step around the lists below.
	if path == "" || path[0] != '/' || strings.Contains(path, "/../") || strings.Contains(path, "//") || strings.HasSuffix(path, "/..") {
		return false
	}
	// The data volume's other spelling for the same folders.
	path = strings.TrimPrefix(path, "/System/Volumes/Data")
	// Apple's own apps live on a volume of their own; other volumes are
	// network shares and removable disks.
	if strings.Contains(path, "/Volumes/") && !strings.HasPrefix(path, "/System/Volumes/Preboot/") {
		return false
	}
	for _, p := range unlookedPaths {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	for _, f := range unlookedFolders {
		if strings.Contains(path, f) {
			return false
		}
	}
	return true
}

func cachedIsFile(path string) bool {
	if !lookedAt(path) {
		return false
	}
	fileCache.Lock()
	if time.Since(fileCache.at) > fileCacheFor {
		fileCache.found, fileCache.at = map[string]bool{}, time.Now()
	}
	if found, ok := fileCache.found[path]; ok {
		fileCache.Unlock()
		return found
	}
	// One look at a time per path: a path that never answers costs one
	// waiting goroutine, not one per listing.
	if fileCache.pending[path] {
		fileCache.Unlock()
		return false
	}
	fileCache.pending[path] = true
	fileCache.Unlock()

	done := make(chan bool, 1)
	go func() {
		info, err := os.Stat(path)
		found := err == nil && info.Mode().IsRegular()
		fileCache.Lock()
		delete(fileCache.pending, path)
		fileCache.found[path] = found
		fileCache.Unlock()
		done <- found
	}()
	select {
	case found := <-done:
		return found
	case <-time.After(statWait):
		return false
	}
}

// ownPath says the path a process was started as is its program's own: an
// absolute path to a file whose name is the kernel's name for the program
// (which the kernel cuts at 16 characters). A process that rewrote it, with
// its arguments after the program or in its place, fails this.
func ownPath(path, comm string) bool {
	if !strings.HasPrefix(path, "/") || comm == "" {
		return false
	}
	base := path[strings.LastIndexByte(path, '/')+1:]
	if base != comm && !(len(comm) >= 16 && strings.HasPrefix(base, comm)) {
		return false
	}
	return isFile(path)
}

// lines calls fn with the fields of each line of a listing.
func lines(out []byte, fn func(f []string)) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		if f := strings.Fields(scanner.Text()); len(f) > 0 {
			fn(f)
		}
	}
}

// psTime reads ps's processor time: [[dd-]hh:]mm:ss.ss.
func psTime(s string) (time.Duration, bool) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, false
	}
	total := seconds
	for i, unit := len(parts)-2, 60.0; i >= 0; i, unit = i-1, unit*60 {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0, false
		}
		total += float64(n) * unit
	}
	total += float64(days) * 86400
	return time.Duration(total * float64(time.Second)), true
}
