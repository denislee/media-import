package importer

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The journal is an append-only, line-based record of an import so an
// interrupted run can be resumed:
//
//	T <files> <bytes>   scan finished (bytes is informational: ReadJournal does not read it)
//	B <src> <dst>       transfer to dst started
//	D <src> <dst>       src fully handled (copied, moved or duplicate)
//
// Fields are tab-separated; paths are quoted with strconv.Quote.

// ResumeState is what a previous, interrupted run left behind.
type ResumeState struct {
	Done    map[string]bool   // sources already handled
	Pending map[string]string // source → destination of transfers that may be incomplete
	Total   int               // files found by the previous scan (0 if unknown)
}

// DoneCount is the number of files the previous run completed.
func (r *ResumeState) DoneCount() int { return len(r.Done) }

// ReadJournal loads a journal written by a previous Run.
func ReadJournal(path string) (*ResumeState, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rs := &ResumeState{Done: map[string]bool{}, Pending: map[string]string{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		switch {
		case len(fields) == 3 && fields[0] == "T":
			rs.Total, _ = strconv.Atoi(fields[1])
		case len(fields) == 3 && (fields[0] == "B" || fields[0] == "D"):
			src, err1 := strconv.Unquote(fields[1])
			dst, err2 := strconv.Unquote(fields[2])
			if err1 != nil || err2 != nil {
				continue // torn final line after a crash
			}
			if fields[0] == "B" {
				rs.Pending[src] = dst
			} else {
				rs.Done[src] = true
				delete(rs.Pending, src)
			}
		}
	}
	return rs, sc.Err()
}

// syncFile fsyncs journal and index files; tests replace it to count calls.
var syncFile = (*os.File).Sync

// journal writes the resume journal with group commit (see groupFile). Only
// B records must be durable at a specific point, before the copy is renamed
// into place; resume treats a lost D or T record as "check again".
type journal struct {
	g *groupFile
}

func openJournal(path string, appendTo bool) (*journal, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if !appendTo {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return nil, err
	}
	return &journal{g: newGroupFile(f)}, nil
}

// write appends one record (not yet synced) and returns its sequence number.
func (j *journal) write(fields ...string) uint64 {
	if j == nil {
		return 0
	}
	return j.g.append(strings.Join(fields, "\t"))
}

func (j *journal) begin(src, dst string) uint64 {
	return j.write("B", strconv.Quote(src), strconv.Quote(dst))
}
func (j *journal) done(src, dst string) { j.write("D", strconv.Quote(src), strconv.Quote(dst)) }

// syncTo makes record seq durable, sharing the fsync with concurrent callers.
func (j *journal) syncTo(seq uint64) error {
	if j == nil || seq == 0 {
		return nil
	}
	return j.g.syncTo(seq)
}

func (j *journal) close() {
	if j != nil {
		_ = j.g.close()
	}
}

// Rebase rewrites source paths recorded under oldRoot to newRoot, for when
// the card was mounted at a different path than during the interrupted run.
func (r *ResumeState) Rebase(oldRoot, newRoot string) {
	if oldRoot == newRoot {
		return
	}
	move := func(p string) string {
		if rel, err := filepath.Rel(oldRoot, p); err == nil && validRel(rel) {
			return filepath.Join(newRoot, rel)
		}
		return p
	}
	done := make(map[string]bool, len(r.Done))
	for p := range r.Done {
		done[move(p)] = true
	}
	pending := make(map[string]string, len(r.Pending))
	for p, d := range r.Pending {
		pending[move(p)] = d
	}
	r.Done, r.Pending = done, pending
}
