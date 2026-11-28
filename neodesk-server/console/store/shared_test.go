package store_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/store"
	"github.com/neodeskapp/neodesk/neodesk-server/console/store/storetest"
	serverdb "github.com/neodeskapp/neodesk/neodesk-server/db"
)

// The console and the signal server share one database and each creates
// the tables both use when it starts first. In both start orders the
// result is the database the Node console and the signal server left in
// that order, and both sides can still work with the shared tables.
func TestSharedDatabaseStartOrders(t *testing.T) {
	orders := []struct {
		name          string
		goServerFirst bool
		dump          string
	}{
		{"go-server-first", true, "go-first"},
		{"console-first", false, "node-first"},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			storetest.Each(t, func(t *testing.T, db *store.DB) {
				if order.goServerFirst {
					storetest.GoServerMigrate(t, db)
					mustMigrate(t, db)
				} else {
					mustMigrate(t, db)
					storetest.GoServerMigrate(t, db)
				}
				got := readCatalog(t, db)

				reference := storetest.SQLite(t)
				if db.IsPostgres() {
					reference = storetest.Postgres(t)
				}
				storetest.ApplyDump(t, reference, readTestdata(t, "dumps/shared-"+db.Dialect().String()+"-"+order.dump+".sql"))
				// The signal server's tables the dump predates:
				// blocklist_entries (L127), server_events (L91), the seven
				// fleet tables, server_instances and the six of sites and
				// their relay nodes
				storetest.GoServerMigrate(t, reference)
				createGoTables(t, reference)
				// On PostgreSQL alert_rules.condition_value is double
				// precision (BUGS AE4); access_tokens has token_kind (BUGS
				// RV1)
				want := readCatalog(t, reference).without(sessionTable).wideConditionValue().withTokenKind(db.IsPostgres()).withLater(db.IsPostgres())
				if n := len(want.tables()); n != 105+len(goTables) {
					t.Fatalf("the shared dump and the Go tables are %d tables besides %s, want %d", n, sessionTable, 105+len(goTables))
				}
				requireSameCatalog(t, "shared database against the dump", want, got)

				// Both migrations again, in both orders: nothing moves.
				mustMigrate(t, db)
				storetest.GoServerMigrate(t, db)
				mustMigrate(t, db)
				requireSameCatalog(t, "shared database after another round of migrations", got, readCatalog(t, db))

				sharedTableOperations(t, db, order.goServerFirst)
			})
		})
	}
}

// Whoever made the users table, the signal server's migration gives it
// credentials_changed_at and the trigger that stamps it, and the console's
// migration before or after it leaves both alone: a password or second
// factor the console changes ends the account's API tokens (RV181).
func TestCredentialsChangedAtInEveryOrigin(t *testing.T) {
	storetest.EachOrigin(t, func(t *testing.T, db *store.DB, origin storetest.Origin) {
		storetest.GoServerMigrate(t, db)
		mustMigrate(t, db)
		ctx := testContext(t)
		shape, err := db.Inspect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		col := shape.Table("users").Column("credentials_changed_at")
		want := store.TypeInteger
		if db.IsPostgres() {
			want = store.TypeBigInt
		}
		if col.Type != want || col.NotNull {
			t.Fatalf("users.credentials_changed_at = %+v, want a nullable %s", col, want)
		}

		mustExec(t, db, `INSERT INTO users (username, password_hash, role, auth_provider) VALUES (?, ?, ?, ?)`,
			"cred-console", "hash-1", "operator", "local")
		changedAt := func() string {
			t.Helper()
			return queryStrings(t, db, `SELECT credentials_changed_at FROM users WHERE username = ?`, "cred-console")[0][0]
		}
		stamped := func(what, stmt string, args ...any) {
			t.Helper()
			mustExec(t, db, `UPDATE users SET credentials_changed_at = NULL WHERE username = ?`, "cred-console")
			before := time.Now().Unix()
			mustExec(t, db, stmt, args...)
			got, err := strconv.ParseInt(changedAt(), 10, 64)
			if err != nil || got < before || got > time.Now().Unix() {
				t.Errorf("%s: credentials changed at %q, want the time of the update", what, changedAt())
			}
		}

		if got := changedAt(); got != "NULL" {
			t.Errorf("new account: credentials changed at %s, want NULL", got)
		}
		mustExec(t, db, `UPDATE users SET role = ? WHERE username = ?`, "viewer", "cred-console")
		if got := changedAt(); got != "NULL" {
			t.Errorf("role change: credentials changed at %s, want NULL", got)
		}
		stamped("password change", `UPDATE users SET password_hash = ? WHERE username = ?`, "hash-2", "cred-console")
		stamped("second factor on", `UPDATE users SET totp_secret = ?, totp_enabled = ? WHERE username = ?`,
			"SECRET", store.BoolArg(shape.Table("users").Type("totp_enabled"), true), "cred-console")
	})
}

// sharedTableOperations writes and reads a user, a device group, a
// strategy, an address book and an audit row from both sides: through the
// signal server's db package and with the SQL the console uses.
func sharedTableOperations(t *testing.T, sdb *store.DB, goServerFirst bool) {
	t.Helper()
	ctx := testContext(t)
	gdb := storetest.GoServer(t, sdb)
	shape, err := sdb.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Users
	if err := gdb.CreateUser(&serverdb.User{Username: "from-go", PasswordHash: "hash", Role: "operator"}); err != nil {
		t.Fatalf("signal server: create user: %v", err)
	}
	consoleUserID, err := sdb.Insert(ctx, `INSERT INTO users (username, password_hash, role, auth_provider) VALUES (?, ?, ?, ?)`,
		"from-console", "hash", "admin", "local")
	if err != nil {
		t.Fatalf("console: create user: %v", err)
	}
	for _, name := range []string{"from-go", "from-console"} {
		u, err := gdb.GetUser(name)
		if err != nil || u == nil || u.Username != name {
			t.Errorf("signal server: read user %s = %+v, %v", name, u, err)
		}
		var role string
		if err := sdb.QueryRow(ctx, `SELECT role FROM users WHERE username = ?`, name).Scan(&role); err != nil || role == "" {
			t.Errorf("console: read user %s = %q, %v", name, role, err)
		}
	}

	// Device groups
	if err := gdb.CreateDeviceGroup(&serverdb.DeviceGroup{GUID: "dg-go", Name: "from go"}); err != nil {
		t.Fatalf("signal server: create device group: %v", err)
	}
	mustExec(t, sdb, `INSERT INTO device_groups (guid, name, note, team_id, source_type, tag_filter) VALUES (?, ?, ?, ?, ?, ?)`,
		"dg-console", "from console", "", "", "manual", "")
	groups, err := gdb.ListDeviceGroups()
	if err != nil {
		t.Fatalf("signal server: list device groups: %v", err)
	}
	seen := map[string]bool{}
	for _, g := range groups {
		seen[g.GUID] = true
	}
	if !seen["dg-go"] || !seen["dg-console"] {
		t.Errorf("signal server sees device groups %v, want dg-go and dg-console", seen)
	}
	if rows := queryStrings(t, sdb, `SELECT guid FROM device_groups WHERE guid IN ('dg-go', 'dg-console') ORDER BY guid`); len(rows) != 2 {
		t.Errorf("console sees device groups %v, want dg-console and dg-go", rows)
	}

	// Strategies
	if err := gdb.CreateStrategy(&serverdb.Strategy{GUID: "st-go", Name: "from go", Enabled: true, Permissions: `{"a":1}`}); err != nil {
		t.Fatalf("signal server: create strategy: %v", err)
	}
	mustExec(t, sdb, `INSERT INTO strategies (guid, name, user_group_guid, device_group_guid, enabled, permissions) VALUES (?, ?, ?, ?, ?, ?)`,
		"st-console", "from console", "", "", store.BoolArg(shape.Table("strategies").Type("enabled"), true), `{"b":2}`)
	strategies, err := gdb.ListStrategies()
	if err != nil {
		t.Fatalf("signal server: list strategies: %v", err)
	}
	seen = map[string]bool{}
	for _, s := range strategies {
		seen[s.GUID] = s.Enabled
	}
	if !seen["st-go"] || !seen["st-console"] {
		t.Errorf("signal server sees enabled strategies %v, want st-go and st-console", seen)
	}
	if rows := queryStrings(t, sdb, `SELECT guid FROM strategies ORDER BY guid`); len(rows) != 2 {
		t.Errorf("console sees strategies %v, want two", rows)
	}

	// Address books. The console keys them by user id on a SQLite table it
	// created itself and by user name everywhere else.
	byName := shape.Table("address_books").Has("username")
	if want := sdb.IsPostgres() || goServerFirst; byName != want {
		t.Errorf("address_books keyed by user name = %v, want %v", byName, want)
	}
	if byName {
		mustExec(t, sdb, `INSERT INTO address_books (username, ab_type, data, updated_at) VALUES (?, ?, ?, `+store.NowSQL(sdb.Dialect())+`)`,
			"from-console", "legacy", `{"peers":[]}`)
	} else {
		mustExec(t, sdb, `INSERT INTO address_books (user_id, ab_type, data, updated_at) VALUES (?, ?, ?, datetime('now'))`,
			consoleUserID, "legacy", `{"peers":[]}`)
	}
	if rows := queryStrings(t, sdb, `SELECT data FROM address_books`); len(rows) != 1 || rows[0][0] != `{"peers":[]}` {
		t.Errorf("console reads address books %v", rows)
	}
	// The signal server follows the shape it finds (BUGS D5)
	if err := gdb.SaveAddressBook("from-go", "legacy", `{"peers":["1"]}`); err != nil {
		t.Errorf("signal server: save address book: %v", err)
	}
	if err := gdb.SaveAddressBook("from-go", "legacy", `{"peers":["2"]}`); err != nil {
		t.Errorf("signal server: save address book again: %v", err)
	}
	if data, err := gdb.GetAddressBook("from-go", "legacy"); err != nil || data != `{"peers":["2"]}` {
		t.Errorf("signal server: read its address book = %q, %v", data, err)
	}
	if data, err := gdb.GetAddressBook("from-console", "legacy"); err != nil || data != `{"peers":[]}` {
		t.Errorf("signal server: read the console's address book = %q, %v", data, err)
	}
	if rows := queryStrings(t, sdb, `SELECT data FROM address_books WHERE data = '{"peers":["2"]}'`); len(rows) != 1 {
		t.Errorf("console reads the signal server's address book %v", rows)
	}
	goUser, err := gdb.GetUser("from-go")
	if err != nil || goUser == nil {
		t.Fatalf("signal server: read user from-go: %v", err)
	}
	if err := gdb.RenameUser(goUser.ID, "from-go-renamed"); err != nil {
		t.Errorf("signal server: rename a user with an address book: %v", err)
	}
	if data, err := gdb.GetAddressBook("from-go-renamed", "legacy"); err != nil || data != `{"peers":["2"]}` {
		t.Errorf("signal server: the renamed user's address book = %q, %v", data, err)
	}

	// Client file transfer rows: the flag is a boolean in the console's
	// PostgreSQL shape and an integer everywhere else; both sides write
	// and read it (BUGS D5)
	if err := gdb.InsertAuditFile(&serverdb.AuditFile{HostID: "100", PeerID: "200", Path: "/a", IsFile: 1}); err != nil {
		t.Errorf("signal server: insert file audit row: %v", err)
	}
	if err := gdb.InsertAuditFile(&serverdb.AuditFile{HostID: "101", PeerID: "200", Path: "/b", IsFile: 0}); err != nil {
		t.Errorf("signal server: insert folder audit row: %v", err)
	}
	mustExec(t, sdb, `INSERT INTO audit_files (host_id, host_uuid, peer_id, direction, path, is_file, num_files, files_json, ip, peer_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"300", "", "400", 0, "/c", store.BoolArg(shape.Table("audit_files").Type("is_file"), false), 1, "[]", "10.0.0.2", "")
	files, err := gdb.ListAuditFiles(serverdb.AuditFilter{Limit: 10})
	if err != nil || len(files) != 3 {
		t.Errorf("signal server lists %d file audit rows, %v, want 3", len(files), err)
	}
	flags := map[string]int{}
	for _, f := range files {
		flags[f.HostID] = f.IsFile
	}
	if flags["100"] != 1 || flags["101"] != 0 || flags["300"] != 0 {
		t.Errorf("signal server reads the flags %v", flags)
	}
	if rows := queryStrings(t, sdb, `SELECT host_id FROM audit_files ORDER BY host_id`); len(rows) != 3 {
		t.Errorf("console reads file audit rows %v", rows)
	}

	// The second factor goes off on either shape of users: the signal
	// server's takes no NULL secret (BUGS D5)
	var secret any
	if shape.Table("users").Column("totp_secret").NotNull {
		secret = ""
	}
	mustExec(t, sdb, `UPDATE users SET totp_secret = ?, totp_recovery_codes = NULL WHERE username = ?`, secret, "from-console")
	if u, err := gdb.GetUser("from-console"); err != nil || u == nil || u.TOTPSecret != "" {
		t.Errorf("signal server: read the user without a second factor = %+v, %v", u, err)
	}

	// Client audit rows
	if err := gdb.InsertAuditConnection(&serverdb.AuditConnection{HostID: "100", PeerID: "200", Action: "new", IP: "10.0.0.1"}); err != nil {
		t.Fatalf("signal server: insert audit row: %v", err)
	}
	mustExec(t, sdb, `INSERT INTO audit_connections (host_id, host_uuid, peer_id, peer_name, action, conn_type, session_id, ip) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"300", "", "400", "", "close", 0, "", "10.0.0.2")
	audits, err := gdb.ListAuditConnections(serverdb.AuditFilter{Limit: 10})
	if err != nil || len(audits) != 2 {
		t.Errorf("signal server lists %d audit rows, %v, want 2", len(audits), err)
	}
	if rows := queryStrings(t, sdb, `SELECT host_id FROM audit_connections ORDER BY host_id`); len(rows) != 2 || rows[0][0] != "100" || rows[1][0] != "300" {
		t.Errorf("console reads audit rows %v", rows)
	}
}
