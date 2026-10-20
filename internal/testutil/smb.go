package testutil

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// SMBFixture runs an isolated authenticated SMB2 server on loopback. It never
// contacts the user's NAS. Opt in with an interpreter that has impacket installed.
func SMBFixture(t *testing.T) (endpoint, directory string) {
	t.Helper()
	python := os.Getenv("XM_SMB_SERVER_PYTHON")
	if python == "" {
		t.Skip("set XM_SMB_SERVER_PYTHON to a Python interpreter with impacket")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	directory = t.TempDir()
	log, err := os.Create(filepath.Join(t.TempDir(), "smb.log"))
	if err != nil {
		t.Fatal(err)
	}
	const script = `
import sys, logging
from impacket import smbserver
from impacket.ntlm import compute_lmhash, compute_nthash
logging.basicConfig(level=logging.ERROR)
s=smbserver.SimpleSMBServer(listenAddress='127.0.0.1',listenPort=int(sys.argv[1]))
s.setSMB2Support(True)
s.addShare('archive',sys.argv[2],readOnly='no')
s.addCredential('fixture-user',0,compute_lmhash('fixture-password'),compute_nthash('fixture-password'))
s.start()
`
	cmd := exec.Command(python, "-c", script, strconv.Itoa(port), directory)
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Close()
		if t.Failed() {
			b, _ := os.ReadFile(log.Name())
			t.Log(string(b))
		}
	})
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return fmt.Sprintf("smb://%s/archive", addr), directory
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("SMB fixture did not start")
	return "", ""
}
