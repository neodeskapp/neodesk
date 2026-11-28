package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/neodeskapp/neodesk/neodesk-server/console/store"
	"github.com/neodeskapp/neodesk/neodesk-server/console/store/storetest"
)

// what one column looks like in the shape
type columnWant struct {
	table, column string
	typ           store.ColumnType
}

// The shape tells the variants of the shared tables apart on every kind of
// database there is: made by the Go console, by the signal server first, or
// by the Node console.
func TestShapeOfEveryVariant(t *testing.T) {
	storetest.EachOrigin(t, func(t *testing.T, db *store.DB, origin storetest.Origin) {
		shape := db.Shape()
		pg := db.IsPostgres()
		goFirst := origin == storetest.GoServerFirst
		if shape.Dialect() != db.Dialect() {
			t.Errorf("shape dialect = %s", shape.Dialect())
		}

		pick := func(sqlite, postgresNode, postgresGo store.ColumnType) store.ColumnType {
			switch {
			case !pg:
				return sqlite
			case goFirst:
				return postgresGo
			}
			return postgresNode
		}
		wants := []columnWant{
			// Keys: 32 bits where the console made the table, 64 bits where the signal server did.
			{"users", "id", pick(store.TypeInteger, store.TypeInteger, store.TypeBigInt)},
			{"folders", "id", pick(store.TypeInteger, store.TypeInteger, store.TypeBigInt)},
			{"device_groups", "id", pick(store.TypeInteger, store.TypeInteger, store.TypeBigInt)},
			{"user_group_members", "user_id", pick(store.TypeInteger, store.TypeInteger, store.TypeBigInt)},
			{"tickets", "id", pick(store.TypeInteger, store.TypeInteger, store.TypeInteger)},
			// Flags
			{"users", "totp_enabled", pick(store.TypeInteger, store.TypeBoolean, store.TypeBoolean)},
			{"audit_files", "is_file", pick(store.TypeInteger, store.TypeBoolean, store.TypeInteger)},
			{"strategies", "enabled", pick(store.TypeInteger, store.TypeBoolean, store.TypeBoolean)},
			{"branding_profiles", "is_active", pick(store.TypeInteger, store.TypeInteger, store.TypeInteger)},
			// Times
			{"users", "created_at", pick(store.TypeText, store.TypeTimestampTZ, store.TypeTimestampTZ)},
			{"dlp_policies", "created_at", pick(store.TypeDateTime, store.TypeTimestampTZ, store.TypeTimestampTZ)},
			{"studio_flows", "created_at", pick(store.TypeText, store.TypeTimestampTZ, store.TypeTimestampTZ)},
			// JSON
			{"audit_files", "files_json", pick(store.TypeText, store.TypeJSONB, store.TypeText)},
			{"audit_alarms", "details", pick(store.TypeText, store.TypeJSONB, store.TypeText)},
			{"strategies", "permissions", pick(store.TypeText, store.TypeJSONB, store.TypeText)},
			{"peer_sysinfo", "raw_json", pick(store.TypeText, store.TypeJSONB, store.TypeText)},
			{"address_books", "data", pick(store.TypeText, store.TypeText, store.TypeText)},
			// Others
			{"peer", "pk", store.TypeBlob},
			{"peer_metrics", "cpu_usage", store.TypeReal},
			{"device_telemetry", "memory_used_bytes", pick(store.TypeInteger, store.TypeBigInt, store.TypeBigInt)},
			{"users", "no_such_column", store.TypeMissing},
			{"no_such_table", "id", store.TypeMissing},
		}
		for _, w := range wants {
			if got := shape.Table(w.table).Type(w.column); got != w.typ {
				t.Errorf("%s.%s is %s, want %s", w.table, w.column, got, w.typ)
			}
		}

		// The address book is keyed by user id only on a SQLite table the
		// console made.
		books := shape.Table("address_books")
		byName := pg || goFirst
		if books.Has("username") != byName || books.Has("user_id") == byName {
			t.Errorf("address_books: username %v, user_id %v, want keyed by name = %v", books.Has("username"), books.Has("user_id"), byName)
		}
		// Columns only one side adds.
		users := shape.Table("users")
		if !users.Has("preferred_language") || !users.Has("guid") || !users.Has("email") {
			t.Error("users lacks a console column")
		}
		if users.Has("is_server_admin") != goFirst {
			t.Errorf("users.is_server_admin present = %v, want %v", users.Has("is_server_admin"), goFirst)
		}
		if got, want := users.Column("totp_secret").NotNull, pg && goFirst; got != want {
			t.Errorf("users.totp_secret NOT NULL = %v, want %v", got, want)
		}

		id := users.Column("id")
		if !id.PrimaryKey || !id.Serial || !id.NotNull && pg {
			t.Errorf("users.id = %+v, want a primary key the database assigns", id)
		}
		if name := users.Column("username"); name.PrimaryKey || name.Serial || !name.NotNull || name.Declared == "" {
			t.Errorf("users.username = %+v", name)
		}
		if role := users.Column("role"); !strings.Contains(role.Default, "'") {
			t.Errorf("users.role default = %q, want a quoted role", role.Default)
		}
		if member := shape.Table("user_group_members").Column("user_id"); !member.PrimaryKey || member.Serial {
			t.Errorf("user_group_members.user_id = %+v, want part of the key and not assigned", member)
		}

		if cols := users.Columns(); len(cols) < 15 || cols[0].Name != "id" || cols[1].Name != "username" {
			t.Errorf("users columns = %v", cols)
		}
		if users.Name() != "users" || !users.Exists() {
			t.Errorf("users: name %q, exists %v", users.Name(), users.Exists())
		}
		if missing := shape.Table("no_such_table"); missing.Exists() || missing.Has("id") || len(missing.Columns()) != 0 || missing.Name() != "no_such_table" {
			t.Errorf("a missing table = %+v", missing)
		}
		if got, want := shape.Table("panel_sessions").Exists(), origin == storetest.NodeFirst; got != want {
			t.Errorf("panel_sessions exists = %v, want %v", got, want)
		}
		if got, want := shape.Table("peers").Exists(), goFirst; got != want {
			t.Errorf("the signal server's peers table exists = %v, want %v", got, want)
		}
		tables := shape.Tables()
		wantTables := map[storetest.Origin]int{storetest.ConsoleFirst: 55, storetest.NodeFirst: 56, storetest.GoServerFirst: 105}[origin] + len(goTables)
		if len(tables) != wantTables || !sortedStrings(tables) {
			t.Errorf("%d tables (sorted %v), want %d", len(tables), sortedStrings(tables), wantTables)
		}

		// What this means for JSON: the id of a new folder is a string where
		// the key is 64 bits wide, as the console always sent it.
		ctx := testContext(t)
		if _, err := db.Insert(ctx, `INSERT INTO folders (name) VALUES (?)`, "Servers"); err != nil {
			t.Fatal(err)
		}
		var folderID store.Int64String
		if err := store.SelectRow(ctx, db, `SELECT id FROM folders`).Scan(&folderID); err != nil {
			t.Fatal(err)
		}
		wantID := "1"
		if pg && goFirst {
			wantID = `"1"`
		}
		if got := marshalJS(t, folderID); got != wantID {
			t.Errorf("folder id in JSON = %s, want %s", got, wantID)
		}
		if got := marshalJS(t, store.NewInt64String(1, shape.Table("folders").Type("id"))); got != wantID {
			t.Errorf("NewInt64String with the shape's type = %s, want %s", got, wantID)
		}
	})
}

func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

// Shape is there after Migrate or Inspect and not before. Inspect picks up
// what changed since, and a Shape that was handed out stays as it was.
func TestShapeLifecycle(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("Shape before Migrate or Inspect did not panic")
				}
			}()
			db.Shape()
		}()

		ctx := testContext(t)
		empty, err := db.Inspect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(empty.Tables()) != 0 || db.Shape() != empty {
			t.Errorf("shape of an empty database has %d tables", len(empty.Tables()))
		}

		mustMigrate(t, db)
		migrated := db.Shape()
		if migrated == empty || len(migrated.Tables()) != 55+len(goTables) || len(empty.Tables()) != 0 {
			t.Errorf("after Migrate: %d tables in the new shape, %d in the old one", len(migrated.Tables()), len(empty.Tables()))
		}

		mustExec(t, db, `ALTER TABLE folders ADD COLUMN extra TEXT`)
		if db.Shape().Table("folders").Has("extra") {
			t.Error("the shape changed without Inspect")
		}
		if _, err := db.Inspect(ctx); err != nil {
			t.Fatal(err)
		}
		if !db.Shape().Table("folders").Has("extra") || migrated.Table("folders").Has("extra") {
			t.Error("Inspect did not pick up the new column, or changed the old shape")
		}

		// Another handle has its own shape.
		other := emptyLike(t, db)
		if _, err := other.Inspect(ctx); err != nil {
			t.Fatal(err)
		}
		if other.Shape().Table("folders").Exists() || !db.Shape().Table("folders").Exists() {
			t.Error("two databases share a shape")
		}
	})
}

// driverColumn is what a driver reports for one column: the type name and
// the Go type of a scanned value.
type driverColumn struct {
	column, typeName, goType string
}

// The value types rely on these answers of the two drivers.
func TestDriverValueTypes(t *testing.T) {
	want := map[store.Dialect][]driverColumn{
		store.SQLite: {
			{"c_text", "TEXT", "string"},
			{"c_integer", "INTEGER", "int64"},
			{"c_real", "REAL", "float64"},
			{"c_blob", "BLOB", "[]uint8"},
			{"c_datetime", "DATETIME", "time.Time"},
		},
		store.Postgres: {
			{"c_text", "TEXT", "string"},
			{"c_integer", "INT4", "int64"},
			{"c_smallint", "INT2", "int64"},
			{"c_bigint", "INT8", "int64"},
			{"c_boolean", "BOOL", "bool"},
			{"c_real", "FLOAT4", "float64"},
			{"c_double", "FLOAT8", "float64"},
			{"c_numeric", "NUMERIC", "string"},
			{"c_timestamptz", "TIMESTAMPTZ", "time.Time"},
			{"c_timestamp", "TIMESTAMP", "time.Time"},
			{"c_date", "DATE", "time.Time"},
			{"c_jsonb", "JSONB", "[]uint8"},
			{"c_json", "JSON", "[]uint8"},
			{"c_bytea", "BYTEA", "[]uint8"},
		},
	}
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		v := vectorsFor(t, db)
		loadValueTable(t, db, v)
		ctx := testContext(t)
		for _, w := range want[db.Dialect()] {
			rows, err := db.Query(ctx, `SELECT `+w.column+` FROM vec WHERE `+w.column+` IS NOT NULL ORDER BY id LIMIT 1`)
			if err != nil {
				t.Fatal(err)
			}
			if !rows.Next() {
				rows.Close()
				t.Fatalf("no value in %s", w.column)
			}
			types, err := rows.ColumnTypes()
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err := rows.Scan(&value); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			got := driverColumn{w.column, types[0].DatabaseTypeName(), fmt.Sprintf("%T", value)}
			if got != w {
				t.Errorf("%s: the driver reports type %q and gives a %s, want %q and %s", w.column, got.typeName, got.goType, w.typeName, w.goType)
			}
		}
		// An expression has no declared type on SQLite and keeps its type on PostgreSQL.
		rows, err := db.Query(ctx, `SELECT COUNT(*), MAX(c_text) FROM vec`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		types, err := rows.ColumnTypes()
		if err != nil {
			t.Fatal(err)
		}
		got := types[0].DatabaseTypeName() + "," + types[1].DatabaseTypeName()
		if want := db.Pick(",", "INT8,TEXT"); got != want {
			t.Errorf("type names of COUNT(*) and MAX(text) = %q, want %q", got, want)
		}
	})
}

// The same answers from the databases a real Node console and signal
// server left behind together, in both start orders (the dumps in
// testdata/dumps), panel_sessions included.
func TestShapeOfSharedDumps(t *testing.T) {
	for _, first := range []string{"go-first", "node-first"} {
		t.Run(first, func(t *testing.T) {
			storetest.Each(t, func(t *testing.T, db *store.DB) {
				storetest.ApplyDump(t, db, readTestdata(t, "dumps/shared-"+db.Dialect().String()+"-"+first+".sql"))
				shape, err := db.Inspect(testContext(t))
				if err != nil {
					t.Fatal(err)
				}
				pg, goFirst := db.IsPostgres(), first == "go-first"
				pick := func(sqlite, postgresNode, postgresGo store.ColumnType) store.ColumnType {
					switch {
					case !pg:
						return sqlite
					case goFirst:
						return postgresGo
					}
					return postgresNode
				}
				for _, w := range []columnWant{
					{"users", "id", pick(store.TypeInteger, store.TypeInteger, store.TypeBigInt)},
					{"device_group_members", "device_group_id", pick(store.TypeInteger, store.TypeInteger, store.TypeBigInt)},
					{"audit_files", "is_file", pick(store.TypeInteger, store.TypeBoolean, store.TypeInteger)},
					{"strategies", "permissions", pick(store.TypeText, store.TypeJSONB, store.TypeText)},
					{"users", "last_login", pick(store.TypeText, store.TypeTimestampTZ, store.TypeTimestampTZ)},
					// The signal server adds this one to a table the console made as well.
					{"users", "is_server_admin", pick(store.TypeInteger, store.TypeBoolean, store.TypeBoolean)},
					{"users", "email", store.TypeText},
					{"peers", "id", store.TypeText},
					{"panel_sessions", "expires_at", pick(store.TypeInteger, store.TypeTimestampTZ, store.TypeTimestampTZ)},
					{"panel_sessions", "created_at", pick(store.TypeInteger, store.TypeBigInt, store.TypeBigInt)},
				} {
					if got := shape.Table(w.table).Type(w.column); got != w.typ {
						t.Errorf("%s.%s is %s, want %s", w.table, w.column, got, w.typ)
					}
				}
				if got, want := shape.Table("address_books").Has("username"), pg || goFirst; got != want {
					t.Errorf("address_books keyed by user name = %v, want %v", got, want)
				}
				if got, want := shape.Table("users").Column("totp_secret").NotNull, pg && goFirst; got != want {
					t.Errorf("users.totp_secret NOT NULL = %v, want %v", got, want)
				}
				if n := len(shape.Tables()); n != 88 {
					t.Errorf("%d tables in the dump, want 88", n)
				}
			})
		})
	}
}

// A database only the signal server has touched: the shape says which of
// the shared tables are there and that the console's columns are not.
func TestShapeOfSignalServerDatabase(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		storetest.GoServerMigrate(t, db)
		shape, err := db.Inspect(testContext(t))
		if err != nil {
			t.Fatal(err)
		}
		users := shape.Table("users")
		if !users.Exists() || users.Has("email") || users.Has("preferred_language") || !users.Has("is_server_admin") {
			t.Errorf("users of the signal server alone: %+v", users.Columns())
		}
		if shape.Table("peer").Exists() || shape.Table("tickets").Exists() || !shape.Table("peers").Exists() {
			t.Error("a console table exists, or the signal server's peers table does not")
		}
		if !shape.Table("folders").Exists() {
			t.Error("no folders: the signal server creates it on both databases (on SQLite since upstream 3.5.264)")
		}
		if !shape.Table("address_books").Has("username") {
			t.Error("the signal server's address_books has no username column")
		}
	})
}
