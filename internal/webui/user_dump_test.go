package webui

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
)

// TestDumpAccountRecoveryPages is the account-side counterpart to
// TestDumpPages. It is off during normal tests; GNIZA_USER_DUMP turns the
// real server-rendered fragments into local files for browser inspection.
func TestDumpAccountRecoveryPages(t *testing.T) {
	dir := os.Getenv("GNIZA_USER_DUMP")
	if dir == "" {
		t.Skip("set GNIZA_USER_DUMP to a directory to dump account pages")
	}
	server, err := New(nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-2 * time.Hour)
	points := []resticrun.Snapshot{
		{ID: "d4c3b2a1", Time: now},
		{ID: "11223344", Time: now.Add(-24 * time.Hour)},
		{ID: "55667788", Time: now.Add(-7 * 24 * time.Hour)},
	}
	views := map[string]userView{
		"overview": {
			Account: "arkady", TotalPoints: 34, Latest: now, Ready: 1,
			Repositories: []userRepository{{
				ID: "backup-server", Name: "BackupServer", Snapshots: 34, Latest: now,
			}},
		},
		"restore": {
			Account: "arkady", Kinds: userKinds, Repository: "backup-server",
			Snapshot: points[0].ID, SnapshotAt: now, Snapshots: points,
		},
		"databases": {
			Account: "arkady", Kinds: userKinds, Repository: "backup-server",
			Snapshot: points[0].ID, SnapshotAt: now, Snapshots: points,
			Kind: granular.KindDatabase, Path: "/backup/arkady/databases",
			Entries: []browseEntry{
				{Name: "arkady_wp.sql", Path: "/backup/arkady/databases/arkady_wp.sql", Item: "arkady_wp", Size: 675_000},
				{Name: "arkady_shop.sql", Path: "/backup/arkady/databases/arkady_shop.sql", Item: "arkady_shop", Size: 18_400_000},
			},
		},
	}
	for name, view := range views {
		view.Downloads, view.Links = true, userLinks{cpanel: true}
		templateName := "user_browse.html"
		path := "/browse"
		if name == "overview" {
			templateName = "user_home.html"
			path = "/"
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "arkady"))
		response := httptest.NewRecorder()
		server.renderUser(response, request, templateName, view)
		if response.Code != http.StatusOK {
			t.Fatalf("render %s = %d: %s", name, response.Code, response.Body.String())
		}
		if err := os.WriteFile(filepath.Join(dir, "account-"+name+".html"), response.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheDatabaseListDoesNotOfferTheGrantsFiles keeps the files that carry
// the account's database users out of the list of databases. They are
// staged in the same directory as the dumps and two of them end in .sql, so
// a lister that goes by the suffix alone offers "_users" and
// "_users-runnable" as though they were databases -- and a restore of one
// would then be pointed at a database of that name, which does not exist.
//
// They are not listed at all, selectable or not. A row nobody can choose,
// named after a file nobody recognises, is the question this started as.
func TestTheDatabaseListDoesNotOfferTheGrantsFiles(t *testing.T) {
	parts := reassemble.Parts{Databases: "/stage/databases"}
	for _, name := range []string{
		granular.DatabaseUsersFile,
		granular.RunnableDatabaseUsersFile,
		granular.DatabaseUsersAuthFile,
	} {
		entry := resticrun.Entry{
			Name: name, Type: "file",
			Path: path.Join(parts.Databases, name),
		}
		if got := itemName(granular.KindDatabase, entry, parts); got != "" {
			t.Fatalf("%s is offered as the database %q", name, got)
		}
		if !databaseUsersPart(name) {
			t.Fatalf("%s is still listed among the databases", name)
		}
	}
	if databaseUsersPart("rtflow_wp.sql") {
		t.Fatal("a real dump was taken for one of the database-user files")
	}

	real := resticrun.Entry{
		Name: "rtflow_wp.sql", Type: "file",
		Path: path.Join(parts.Databases, "rtflow_wp.sql"),
	}
	if got := itemName(granular.KindDatabase, real, parts); got != "rtflow_wp" {
		t.Fatalf("a real dump listed as %q", got)
	}
}
