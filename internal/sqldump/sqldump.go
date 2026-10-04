// Package sqldump says what a database dump would put back, without
// loading it.
package sqldump

import (
	"bytes"
	"errors"
	"io"
)

// State is what a dump would restore.
type State int

const (
	// Cut is a dump that creates nothing and does not say it finished:
	// nothing at all, or the head of a dump whose writer was stopped. A
	// database put back from one comes back empty when it was not.
	Cut State = iota
	// Holds is a dump with something in it to create.
	Holds
	// Empty is a whole dump of a database that has nothing in it. That
	// is a faithful backup: seven such databases on two servers failed
	// their accounts' rehearsals on 2026-10-02, each with no table in it
	// on the live server.
	Empty
)

// finished is what mysqldump and mariadb-dump write once everything
// else has been written: the comment that says so, and, on a dump taken
// without comments, the last of the settings they put back.
var finished = [][]byte{
	[]byte("-- DUMP COMPLETED"),
	[]byte("SQL_NOTES=@OLD_SQL_NOTES"),
}

// tail is how much of the end of a dump is kept to look for that in.
const tail = 4 << 10

// Scan reads a dump to its end, or to the first thing it creates.
//
// In blocks, never whole: a dump is the largest file an account has, and
// this runs on the server the account lives on.
func Scan(r io.Reader) (State, error) {
	word := []byte("CREATE")
	buf := make([]byte, tail+64<<10)
	carried := 0
	for {
		read, err := r.Read(buf[carried:])
		if read > 0 {
			block := buf[:carried+read]
			fresh := block[carried:]
			for i, b := range fresh {
				if 'a' <= b && b <= 'z' {
					fresh[i] = b - ('a' - 'A')
				}
			}
			// What was carried has been searched already, but a word
			// lying across the boundary has not.
			from := max(0, carried-(len(word)-1))
			if bytes.Contains(block[from:], word) {
				return Holds, nil
			}
			keep := min(len(block), tail)
			copy(buf, block[len(block)-keep:])
			carried = keep
		}
		if errors.Is(err, io.EOF) {
			for _, mark := range finished {
				if bytes.Contains(buf[:carried], mark) {
					return Empty, nil
				}
			}
			return Cut, nil
		}
		if err != nil {
			return Cut, err
		}
	}
}
