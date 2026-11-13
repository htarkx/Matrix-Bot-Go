package hostwatch

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// procfs reads the files the module needs under a root (normally /proc;
// a testdata directory in tests).
type procfs string

func (p procfs) path(parts ...string) string {
	return filepath.Join(append([]string{string(p)}, parts...)...)
}

// cpuSample are the cumulative jiffies of the "cpu" line of /proc/stat.
type cpuSample struct {
	total, idle, steal uint64
}

func (p procfs) cpu() (cpuSample, error) {
	f, err := os.Open(p.path("stat"))
	if err != nil {
		return cpuSample{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuSample{}, errors.New("empty stat")
	}
	// cpu user nice system idle iowait irq softirq steal guest guest_nice
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, fmt.Errorf("unexpected stat line %q", sc.Text())
	}
	var v [8]uint64
	for i := 0; i < 8 && i+1 < len(fields); i++ {
		if v[i], err = strconv.ParseUint(fields[i+1], 10, 64); err != nil {
			return cpuSample{}, err
		}
	}
	var s cpuSample
	for _, x := range v {
		s.total += x
	}
	s.idle = v[3] + v[4] // idle + iowait
	s.steal = v[7]
	return s, nil
}

// keyValues parses "key[:] value [kB]" files such as meminfo and vmstat;
// kB values are returned in bytes.
func (p procfs) keyValues(name string) (map[string]uint64, error) {
	b, err := os.ReadFile(p.path(name))
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(strings.Replace(line, ":", " ", 1))
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 2 && f[2] == "kB" {
			v *= 1024
		}
		out[f[0]] = v
	}
	return out, nil
}

// swapPages is pswpin + pswpout since boot.
func (p procfs) swapPages() (uint64, error) {
	vm, err := p.keyValues("vmstat")
	if err != nil {
		return 0, err
	}
	return vm["pswpin"] + vm["pswpout"], nil
}

// meminfo values in bytes.
func (p procfs) meminfo() (map[string]uint64, error) { return p.keyValues("meminfo") }

// psi returns the "some" averages of /proc/pressure/<kind>, empty when the
// kernel has no PSI.
func (p procfs) psi(kind string) map[string]float64 {
	b, err := os.ReadFile(p.path("pressure", kind))
	if err != nil {
		return nil
	}
	out := map[string]float64{}
	line, _, _ := strings.Cut(string(b), "\n")
	for _, kv := range strings.Fields(line)[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if f, err := strconv.ParseFloat(v, 64); ok && err == nil {
			out[k] = f
		}
	}
	return out
}

func (p procfs) loadavg() ([3]float64, error) {
	var l [3]float64
	b, err := os.ReadFile(p.path("loadavg"))
	if err != nil {
		return l, err
	}
	_, err = fmt.Sscan(string(b), &l[0], &l[1], &l[2])
	return l, err
}

func (p procfs) uptime() (float64, error) {
	b, err := os.ReadFile(p.path("uptime"))
	if err != nil {
		return 0, err
	}
	var up float64
	_, err = fmt.Sscan(string(b), &up)
	return up, err
}

// proc is one process from /proc/<pid>/stat.
type proc struct {
	comm  string
	ticks uint64 // utime + stime
	rss   uint64 // bytes
}

// procs snapshots every process. Processes that exit mid-scan are skipped.
func (p procfs) procs() map[int]proc {
	entries, _ := os.ReadDir(string(p))
	page := uint64(os.Getpagesize())
	out := make(map[int]proc, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(p.path(e.Name(), "stat"))
		if err != nil {
			continue
		}
		s := string(raw)
		// comm may contain spaces and parentheses: it ends at the LAST ")"
		open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || end < open || end+2 > len(s) {
			continue
		}
		f := strings.Fields(s[end+2:])
		// after "(comm) ": state=0 ... utime=11 stime=12 ... rss=21 (pages)
		if len(f) < 22 {
			continue
		}
		ut, _ := strconv.ParseUint(f[11], 10, 64)
		st, _ := strconv.ParseUint(f[12], 10, 64)
		rss, _ := strconv.ParseUint(f[21], 10, 64)
		out[pid] = proc{comm: s[open+1 : end], ticks: ut + st, rss: rss * page}
	}
	return out
}

// clockTicks is USER_HZ, 100 on every Linux architecture Go supports.
const clockTicks = 100

// diskUsage mirrors Python's shutil.disk_usage.
type diskUsage struct{ total, used, free uint64 }

func statDisk(path string) (diskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return diskUsage{}, err
	}
	bs := uint64(st.Bsize)
	return diskUsage{
		total: st.Blocks * bs,
		used:  (st.Blocks - st.Bfree) * bs,
		free:  st.Bavail * bs,
	}, nil
}

func (d diskUsage) pct() float64 {
	if d.total == 0 {
		return 0
	}
	return 100 * float64(d.used) / float64(d.total)
}
