package resticrun

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxPlanBytes is how much of a retention plan is read before it is
// given up on. restic names every snapshot three times over in one, with
// its paths and its summary each time, so a repository of six thousand
// snapshots says tens of megabytes; this is room for a hundred times
// that, and a bound all the same.
const maxPlanBytes = 1 << 30

// errPlanTooLarge is a plan that went past maxPlanBytes.
var errPlanTooLarge = errors.New("resticrun: the retention plan was too large to read in full")

// planStream reads a retention plan as restic writes it.
//
// It used to be captured whole and then parsed, under the cap every
// command's output is captured under. On a repository where retention
// had not run for three weeks the plan was larger than the cap, so the
// plan could not be read, so retention could not run, so the plan grew.
// Read as it arrives, what is kept is a line for each snapshot and not
// what restic said about it.
type planStream struct {
	writer *io.PipeWriter
	done   chan struct{}

	groups []retentionGroupJSON
	read   int64
	err    error
}

func newPlanStream() *planStream {
	reader, writer := io.Pipe()
	stream := &planStream{writer: writer, done: make(chan struct{})}
	go func() {
		defer close(stream.done)
		counted := &countingReader{from: reader, limit: maxPlanBytes}
		stream.groups, stream.err = decodeForgetGroups(counted)
		// Whatever is left is read and dropped: restic is writing into
		// this, and would wait for ever on a reader that had gone.
		_, _ = io.Copy(io.Discard, reader)
		stream.read = counted.read
		if counted.over {
			stream.groups, stream.err = nil, errPlanTooLarge
		}
	}()
	return stream
}

func (s *planStream) Write(p []byte) (int, error) { return s.writer.Write(p) }

// finish says what was read once restic has exited. captured is what the
// command's own result carries, which is where the plan is when whatever
// ran restic did not write it to the stream.
func (s *planStream) finish(captured []byte) ([]retentionGroupJSON, error) {
	_ = s.writer.Close()
	<-s.done
	if s.read == 0 && len(bytes.TrimSpace(captured)) > 0 {
		return decodeForgetGroups(bytes.NewReader(captured))
	}
	return s.groups, s.err
}

// countingReader stops a reader at a limit and remembers that it did.
type countingReader struct {
	from  io.Reader
	limit int64
	read  int64
	over  bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.read >= c.limit {
		c.over = true
		return 0, errPlanTooLarge
	}
	if remaining := c.limit - c.read; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.from.Read(p)
	c.read += int64(n)
	return n, err
}

// decodeForgetGroups reads what "restic forget --json" said, one group
// at a time.
//
// restic writes one of two things: the plan, as an array of groups, or a
// single object saying why it could not run at all. The second is what a
// stale lock looks like, and reading it as an empty plan would report
// nothing to remove for a repository whose retention has never run.
func decodeForgetGroups(from io.Reader) ([]retentionGroupJSON, error) {
	reader := bufio.NewReader(from)
	first, err := firstByte(reader)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resticrun: read the retention plan: %w", err)
	}
	decoder := json.NewDecoder(reader)
	if first == '{' {
		var failure struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := decoder.Decode(&failure); err == nil && failure.Message != "" {
			if failure.Code == exitLocked {
				return nil, fmt.Errorf("%w: forget: %s", ErrLocked, failure.Message)
			}
			return nil, fmt.Errorf("resticrun: forget: %s", failure.Message)
		}
		return nil, fmt.Errorf("resticrun: forget said something unreadable")
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("resticrun: read the retention plan: %w", err)
	}
	var groups []retentionGroupJSON
	for decoder.More() {
		var group retentionGroupJSON
		if err := decoder.Decode(&group); err != nil {
			return nil, fmt.Errorf("resticrun: read the retention plan: %w", err)
		}
		groups = append(groups, group)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("resticrun: read the retention plan: %w", err)
	}
	return groups, nil
}

// firstByte is the first byte that is not white space, left unread.
func firstByte(reader *bufio.Reader) (byte, error) {
	for {
		next, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		switch next {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return next, reader.UnreadByte()
	}
}
