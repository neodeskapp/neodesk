package backup

import "os"

// The parts of a full backup, in the order the contents panel lists them.
const (
	PartDatabase        = "database"         // console/data.json: every table
	PartServerData      = "server_data"      // goserver/data.json: what the API server says
	PartDatabaseFile    = "database_file"    // the SQLite file, as the console's and the server's
	PartEnv             = "env"              // the console's .env
	PartSessionSecret   = "session_secret"   // DATA_DIR/.session_secret
	PartServerKeys      = "server_keys"      // the key pair and the API key
	PartMeshCert        = "mesh_cert"        // the mesh agent certificate
	PartUploads         = "uploads"          // DATA_DIR/uploads
	PartChatFiles       = "chat_files"       // the chat files and their links
	PartAttachments     = "attachments"      // the ticket attachments
	PartFonts           = "fonts"            // the installed fonts
	PartPublicEndpoints = "public_endpoints" // the public endpoints saved in the console
	PartTLS             = "tls"              // the console's TLS certificate, key and CA
)

// Why a part is left out of a backup made now.
const (
	// ReasonDocker: the Docker image takes its settings from the
	// container's environment, there is no environment file to save.
	ReasonDocker = "docker"
	// ReasonEnvironment: SESSION_SECRET sets the session secret, there is
	// no file of it.
	ReasonEnvironment = "environment"
	// ReasonPostgres: the database is PostgreSQL; its rows are in the data
	// dump, there is no database file.
	ReasonPostgres = "postgres"
	// ReasonNotConfigured: no path is set for it (the TLS files).
	ReasonNotConfigured = "not_configured"
	// ReasonNoServer: the console has no API server to ask.
	ReasonNoServer = "no_server"
	// ReasonEmpty: the folder has no files.
	ReasonEmpty = "empty"
	// ReasonMissing: the file is not there or cannot be read.
	ReasonMissing = "missing"
)

// partOrder lists every part once, with whether it holds secrets (keys,
// passwords, or their hashes and second-factor secrets).
var partOrder = []struct {
	id     string
	secret bool
}{
	{PartDatabase, true},
	{PartServerData, false},
	{PartDatabaseFile, true},
	{PartEnv, true},
	{PartSessionSecret, true},
	{PartServerKeys, true},
	{PartMeshCert, true},
	{PartUploads, false},
	{PartChatFiles, false},
	{PartAttachments, false},
	{PartFonts, false},
	{PartPublicEndpoints, false},
	{PartTLS, true},
}

// planEntry is a file or folder a full backup takes from disk: its part,
// its name in the archive (for a folder the prefix of its files) and
// where it is. An empty path is no file.
type planEntry struct {
	part, name, path string
	folder           bool
}

// plan is what a full backup takes from disk besides the two dumps, in
// archive order. CreateFullBackup adds these entries and FullBackupStats
// counts them, so the contents answer is what the archive holds.
//
// A Docker install saves no .env: the image takes its settings from the
// container's environment (the compose file's .env is on the host), and a
// .env in the container is only the console's copy of the public
// endpoints, which nothing reads at the start and the endpoints file
// already holds.
func (s *Service) plan(p paths) []planEntry {
	env := p.env
	if s.o.Docker {
		env = ""
	}
	entries := []planEntry{
		{part: PartEnv, name: "console/.env", path: env},
		{part: PartSessionSecret, name: "console/.session_secret", path: p.sessionSecret},
		{part: PartDatabaseFile, name: "console/auth.db", path: p.consoleDB},
		{part: PartUploads, name: uploadsPrefix, path: p.uploadsDir, folder: true},
		{part: PartChatFiles, name: chatFilesPrefix, path: p.chatFilesDir, folder: true},
		{part: PartAttachments, name: attachmentsPrefix, path: p.attachmentsDir, folder: true},
		{part: PartFonts, name: fontsPrefix, path: p.fontsDir, folder: true},
		{part: PartPublicEndpoints, name: endpointsEntry, path: p.endpointsFile},
	}
	for _, f := range tlsEntries(p) {
		entries = append(entries, planEntry{part: PartTLS, name: f.name, path: f.target})
	}
	return append(entries,
		planEntry{part: PartServerKeys, name: "goserver/id_ed25519", path: p.goPrivKey},
		planEntry{part: PartServerKeys, name: "goserver/id_ed25519.pub", path: p.goPubKey},
		planEntry{part: PartServerKeys, name: "goserver/.api_key", path: p.goAPIKey},
		planEntry{part: PartServerKeys, name: "goserver/node_control.crt", path: p.nodeControlCert},
		planEntry{part: PartServerKeys, name: "goserver/node_control.key", path: p.nodeControlKey},
		planEntry{part: PartDatabaseFile, name: "goserver/db_v2.sqlite3", path: p.goDB},
		planEntry{part: PartMeshCert, name: "goserver/mesh_agent_server.pem", path: p.meshAgentCert},
	)
}

// readableFile says a backup takes the file at path: a regular file that
// opens for reading.
func readableFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// folderFiles counts the files below dir a backup takes, walked as
// CreateFullBackup walks them.
func folderFiles(dir string) int {
	if dir == "" {
		return 0
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return 0
	}
	n := 0
	_ = walkFiles(dir, "", func(_, abs string) {
		if readableFile(abs) {
			n++
		}
	})
	return n
}

// parts is what a backup made now holds, part by part. The dumps are made
// with the backup: the console's data counts as one file, the server's
// when there is an API server to ask. The SQLite file counts as it is on
// disk; the backup takes a copy of it (VACUUM INTO).
func (s *Service) parts(p paths) []Part {
	files := map[string]int{PartDatabase: 1}
	if s.o.GoAPI != nil {
		files[PartServerData] = 1
	}
	for _, e := range s.plan(p) {
		switch {
		case e.folder:
			files[e.part] += folderFiles(e.path)
		case readableFile(e.path):
			files[e.part]++
		}
	}
	out := make([]Part, 0, len(partOrder))
	for _, o := range partOrder {
		part := Part{ID: o.id, Files: files[o.id], Included: files[o.id] > 0, Secret: o.secret}
		if !part.Included {
			part.Reason = s.whyLeftOut(o.id, p)
		}
		out = append(out, part)
	}
	return out
}

// whyLeftOut is why a part has no file in a backup made now.
func (s *Service) whyLeftOut(id string, p paths) string {
	switch id {
	case PartServerData:
		return ReasonNoServer
	case PartEnv:
		if s.o.Docker {
			return ReasonDocker
		}
	case PartSessionSecret:
		if s.o.SessionSecretFromEnv {
			return ReasonEnvironment
		}
	case PartDatabaseFile:
		if p.goDB == "" {
			return ReasonPostgres
		}
	case PartTLS:
		if p.certFile == "" && p.keyFile == "" && p.caFile == "" {
			return ReasonNotConfigured
		}
	case PartUploads, PartChatFiles, PartAttachments, PartFonts:
		return ReasonEmpty
	}
	return ReasonMissing
}
