package sqldump

import (
	"strings"
	"testing"
	"testing/iotest"
)

// What mariadb-dump wrote for dibrot_new on 182.54.236.10, a database
// with no table in it.
const emptyDatabase = `/*M!999999\- enable the sandbox mode */
-- MariaDB dump 10.19  Distrib 10.6.20-MariaDB, for Linux (x86_64)
--
-- Host: localhost    Database: dibrot_new
-- ------------------------------------------------------
-- Server version	10.6.20-MariaDB

/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
/*!40111 SET @OLD_SQL_NOTES=@@SQL_NOTES, SQL_NOTES=0 */;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

-- Dump completed on 2026-10-04  3:33:03
`

func TestADumpSaysWhatItWouldPutBack(t *testing.T) {
	head := emptyDatabase[:strings.Index(emptyDatabase, "/*!40103 SET TIME_ZONE=@OLD")]
	withoutComments := strings.ReplaceAll(emptyDatabase, "-- Dump completed on 2026-10-04  3:33:03\n", "")
	padding := strings.Repeat("-- a comment that says nothing\n", 20000)
	for name, c := range map[string]struct {
		dump string
		want State
	}{
		"nothing at all":                          {"", Cut},
		"a database with no table in it":          {emptyDatabase, Empty},
		"the same, taken without comments":        {withoutComments, Empty},
		"the head of a dump that was stopped":     {head, Cut},
		"a long dump that was stopped":            {head + padding, Cut},
		"a long dump of nothing":                  {padding + emptyDatabase, Empty},
		"a table":                                 {head + "CREATE TABLE `a` (id int);\n", Holds},
		"a table, in small letters":               {head + "create table a (id int);\n", Holds},
		"a table a long way in":                   {head + padding + "CREATE TABLE `a` (id int);\n", Holds},
		"a table in a dump that was then stopped": {head + "CREATE TABLE `a` (id int);\nINSERT INTO", Holds},
	} {
		// One byte at a time and in whatever pieces the reader likes:
		// neither word may be lost at a boundary.
		for way, reader := range map[string]func() (State, error){
			"whole":   func() (State, error) { return Scan(strings.NewReader(c.dump)) },
			"by byte": func() (State, error) { return Scan(iotest.OneByteReader(strings.NewReader(c.dump))) },
			"by half": func() (State, error) { return Scan(iotest.HalfReader(strings.NewReader(c.dump))) },
		} {
			got, err := reader()
			if err != nil || got != c.want {
				t.Errorf("%s, read %s: %v (%v), want %v", name, way, got, err, c.want)
			}
		}
	}
}
