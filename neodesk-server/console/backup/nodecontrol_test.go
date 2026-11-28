package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestFullBackupCarriesTheRelayNodeCertificate: relay nodes pin the
// server's node control certificate, so a full backup takes it and a
// restore with secrets puts it back; without it every node would refuse
// the restored server.
func TestFullBackupCarriesTheRelayNodeCertificate(t *testing.T) {
	src := newInstall(t, "sqlite")
	src.seed(t)
	files := map[string]string{
		"node_control.crt": "-----BEGIN CERTIFICATE-----\nnode control\n",
		"node_control.key": "-----BEGIN PRIVATE KEY-----\nnode control\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(src.keysPath, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive, _, err := src.service(t, "x").CreateFullBackup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inArchive := map[string]bool{}
	for _, e := range tarEntries(t, archive) {
		inArchive[e.name] = true
	}
	for name := range files {
		if !inArchive["goserver/"+name] {
			t.Errorf("backup has no goserver/%s", name)
		}
	}

	dst := newInstall(t, "sqlite")
	if _, err := dst.service(t, "x").RestoreFullBackup(context.Background(), archive, RestoreOptions{Secrets: true}); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		got, err := os.ReadFile(filepath.Join(dst.keysPath, name))
		if err != nil || string(got) != body {
			t.Errorf("%s after restore: %q, %v; want the backed-up file", name, got, err)
		}
	}
}
