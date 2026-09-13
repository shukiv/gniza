package plain_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/plain"
)

// TestTheRolesAreListedWithWhatTheyReach: every PostgreSQL role is a
// candidate beside the databases, with what authenticates it, its
// attributes, what it owns and what it can reach; a role attached is
// kept with every source dumping a database it owns or reaches, and
// the same refusals apply as for a MySQL user.
func TestTheRolesAreListedWithWhatTheyReach(t *testing.T) {
	psql, pgdump := fakePostgres(t, "erp", "crm")
	provider := &plain.Provider{Catalog: newCatalog(), PsqlPath: psql, PgDumpPath: pgdump, PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "erp", PostgreSQL: []string{"erp"}})
	found, err := provider.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, role := range found.PostgreSQLUsers {
		item := role.Who() + ":" + role.Plugin
		if role.System {
			item += ":system"
		}
		if len(role.Global) > 0 {
			item += ":global=" + strings.Join(role.Global, "+")
		}
		for _, right := range role.Rights {
			item += ":" + right.Database + "=" + strings.Join(right.Privileges, "+")
		}
		if role.AttachedTo != "" {
			item += ":with=" + role.AttachedTo
		}
		roles = append(roles, item)
	}
	want := "crm_owner:scram-sha-256:crm=OWNER erp_owner:scram-sha-256:erp=OWNER " +
		"reporter:md5:global=CREATEROLE:crm=CONNECT:erp=CONNECT " +
		"postgres:no password:system:global=CREATEDB+SUPERUSER+CREATEROLE+REPLICATION+BYPASSRLS"
	if got := strings.Join(roles, " "); got != want {
		t.Errorf("roles = %q, want %q", got, want)
	}
	if erp := found.PostgreSQL[1]; erp.Name != "erp" || strings.Join(erp.Users, ",") != "erp_owner" || len(erp.Rights) != 2 ||
		erp.Rights[0].Who() != "erp_owner" || erp.Rights[1].Who() != "reporter" || erp.Rights[1].Privileges[0] != "CONNECT" {
		t.Errorf("the erp database says %+v", erp)
	}

	names, err := provider.AttachPostgreSQLUser(context.Background(), "erp_owner")
	if err != nil || strings.Join(names, ",") != "erp" {
		t.Errorf("attaching erp_owner = %v, %v", names, err)
	}
	if again, err := provider.AttachPostgreSQLUser(context.Background(), "erp_owner"); err != nil || strings.Join(again, ",") != "erp" {
		t.Errorf("attaching erp_owner twice = %v, %v", again, err)
	}
	if names, err := provider.AttachPostgreSQLUser(context.Background(), "reporter"); err != nil || strings.Join(names, ",") != "erp" {
		t.Errorf("attaching reporter = %v, %v", names, err)
	}
	found, _ = provider.Candidates(context.Background())
	if found.PostgreSQLUsers[1].AttachedTo != "erp" || found.PostgreSQLUsers[2].AttachedTo != "erp" {
		t.Errorf("after attaching, the roles are with %q and %q", found.PostgreSQLUsers[1].AttachedTo, found.PostgreSQLUsers[2].AttachedTo)
	}
	sources, _ := provider.Sources(context.Background())
	if strings.Join(sources[0].PostgreSQLUsers, ",") != "erp_owner,reporter" {
		t.Errorf("the source keeps %v", sources[0].PostgreSQLUsers)
	}
	for role, complaint := range map[string]string{
		"crm_owner": "has rights on no database that is backed up; tick crm with it",
		"postgres":  "the server's own role",
		"nobody":    "has no PostgreSQL role nobody",
		"x'y":       "not a role name",
	} {
		if _, err := provider.AttachPostgreSQLUser(context.Background(), role); err == nil || !strings.Contains(err.Error(), complaint) {
			t.Errorf("attaching %s = %v, want %q", role, err, complaint)
		}
	}
}

// TestTheRolesAreKeptBesideTheDumpsAndMadeBeforeTheLoad: a backup of a
// source with roles writes them beside the dumps, as data and as
// statements, with the verifier each had, what it owns and holds on
// the source's databases, and the memberships among them; what a
// restore does not give back is said. Loading the dump makes the roles
// first, against the postgres database, and a file that names a
// database the source was not chosen with is refused.
func TestTheRolesAreKeptBesideTheDumpsAndMadeBeforeTheLoad(t *testing.T) {
	psql, pgdump := fakePostgres(t, "erp")
	provider := &plain.Provider{Catalog: newCatalog(), PsqlPath: psql, PgDumpPath: pgdump, PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "erp", PostgreSQL: []string{"erp"}})
	for _, role := range []string{"erp_owner", "reporter"} {
		if _, err := provider.AttachPostgreSQLUser(context.Background(), role); err != nil {
			t.Fatal(err)
		}
	}
	staging := t.TempDir()
	payload, err := provider.Stage(context.Background(), panel.StageRequest{
		Account: panel.AccountInfo{User: "erp"}, StagingDir: staging, Mode: pkgacct.ModeSplit})
	if err != nil {
		t.Fatal(err)
	}
	dumps := filepath.Join(staging, "metadata", plain.DumpDir)
	runnable, err := os.ReadFile(filepath.Join(dumps, "_roles-runnable.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`DO $gniza$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_owner') THEN CREATE ROLE "erp_owner" WITH LOGIN PASSWORD 'SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy'; END IF; END $gniza$;`,
		`ALTER DATABASE "erp" OWNER TO "erp_owner";`,
		`CREATE ROLE "reporter" WITH LOGIN PASSWORD 'md5d41d8cd98f00b204e9800998ecf8427e'; END IF; END $gniza$;`,
		`GRANT CONNECT ON DATABASE "erp" TO "reporter";`,
		`GRANT "reporter" TO "erp_owner";`,
		"-- reporter: not put back by a restore: CREATEROLE",
	} {
		if !strings.Contains(string(runnable), want) {
			t.Errorf("the runnable file lacks %q:\n%s", want, runnable)
		}
	}
	if strings.Contains(string(runnable), "CREATEROLE;") {
		t.Errorf("the runnable file gives CREATEROLE back:\n%s", runnable)
	}
	data, err := os.ReadFile(filepath.Join(dumps, "_roles.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"erp_owner","login":true,"password":"SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy","owns":["erp"],"member_of":["reporter"]},` +
		`{"name":"reporter","login":true,"password":"md5d41d8cd98f00b204e9800998ecf8427e","left_out":["CREATEROLE"],"rights":[{"database":"erp","privileges":["CONNECT"]}]}]`
	if string(data) != want {
		t.Errorf("the data file = %s\nwant %s", data, want)
	}
	record, err := os.ReadFile(filepath.Join(staging, "metadata", plain.RecordDir, plain.RecordFile))
	if err != nil || !strings.Contains(string(record), `"postgresql_users"`) || !strings.Contains(string(record), `"reporter"`) {
		t.Errorf("the record = %s, %v", record, err)
	}
	var left []string
	for _, omission := range payload.Missing {
		left = append(left, omission.Why)
	}
	if len(left) != 1 || left[0] != "reporter: not put back by a restore: CREATEROLE" {
		t.Errorf("what is left out is not said: %v", left)
	}

	// The restore runs the statements for the database being loaded,
	// before the dump goes in.
	if err := provider.LoadDatabase(context.Background(), "erp", "erp.pg", filepath.Join(dumps, "erp.pg.sql")); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(filepath.Join(filepath.Dir(psql), "psql.log"))
	log := string(logged)
	roles, load := strings.Index(log, `OWNER TO "erp_owner"`), strings.Index(log, "-d erp\n")
	if roles < 0 || load < 0 || roles > load || !strings.Contains(log[:load], "-d postgres") || !strings.Contains(log[load:], "-- pg dump of erp") {
		t.Errorf("the roles were not made before the load:\n%s", log)
	}

	// A file that grants what is not a privilege on a database is
	// refused, not run: the file is read back by a later restore and
	// every word in it is checked before it reaches the server.
	var kept []map[string]any
	if err := json.Unmarshal(data, &kept); err != nil {
		t.Fatal(err)
	}
	kept[1]["rights"] = []map[string]any{{"database": "erp", "privileges": []string{"ALL PRIVILEGES"}}}
	tampered, _ := json.Marshal(kept)
	if err := os.WriteFile(filepath.Join(dumps, "_roles.json"), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provider.LoadDatabase(context.Background(), "erp", "erp.pg", filepath.Join(dumps, "erp.pg.sql")); err == nil ||
		!strings.Contains(err.Error(), `"ALL PRIVILEGES" is not a privilege on a database`) {
		t.Errorf("a file granting what is not a privilege = %v", err)
	}
}
