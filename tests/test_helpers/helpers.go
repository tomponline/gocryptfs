package test_helpers

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/rfjakob/gocryptfs/internal/nametransform"
)

// Note: the code assumes that all have a trailing slash
const TmpDir = "/tmp/gocryptfs_main_test/"
const DefaultPlainDir = TmpDir + "plain/"
const DefaultCipherDir = TmpDir + "cipher/"

const GocryptfsBinary = "../../gocryptfs"

// ResetTmpDir - delete old tmp dir, create new one, write gocryptfs.diriv
func ResetTmpDir(plaintextNames bool) {

	// Try to unmount everything
	entries, err := ioutil.ReadDir(TmpDir)
	if err == nil {
		for _, e := range entries {
			fu := exec.Command("fusermount", "-z", "-u", filepath.Join(TmpDir, e.Name()))
			fu.Run()
		}
	}

	err = os.RemoveAll(TmpDir)
	if err != nil {
		fmt.Println("resetTmpDir: RemoveAll:" + err.Error())
		os.Exit(1)
	}

	err = os.MkdirAll(DefaultPlainDir, 0777)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	err = os.MkdirAll(DefaultCipherDir, 0777)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if !plaintextNames {
		err = nametransform.WriteDirIV(DefaultCipherDir)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	}
}

// Mount CIPHERDIR "c" on PLAINDIR "p"
func Mount(c string, p string, extraArgs ...string) error {
	var args []string
	args = append(args, extraArgs...)
	args = append(args, "-nosyslog", "-q", "-wpanic")
	//args = append(args, "-fusedebug")
	//args = append(args, "-d")
	args = append(args, c)
	args = append(args, p)
	cmd := exec.Command(GocryptfsBinary, args...)
	cmd.Stderr = os.Stderr
	if testing.Verbose() {
		cmd.Stdout = os.Stdout
	}
	return cmd.Run()
}

// MountOrExit calls mount() and exits on failure.
func MountOrExit(c string, p string, extraArgs ...string) {
	err := Mount(c, p, extraArgs...)
	if err != nil {
		fmt.Printf("mount failed: %v", err)
		os.Exit(1)
	}
}

// MountOrFatal calls mount() and calls t.Fatal() on failure.
func MountOrFatal(t *testing.T, c string, p string, extraArgs ...string) {
	err := Mount(c, p, extraArgs...)
	if err != nil {
		t.Fatal(fmt.Errorf("mount failed: %v", err))
	}
}

// Unmount PLAINDIR "p"
func Unmount(p string) error {
	fu := exec.Command("fusermount", "-u", "-z", p)
	fu.Stdout = os.Stdout
	fu.Stderr = os.Stderr
	err := fu.Run()
	if err != nil {
		fmt.Println(err)
	}
	return err
}

// Return md5 string for file "filename"
func Md5fn(filename string) string {
	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		fmt.Printf("ReadFile: %v\n", err)
		return ""
	}
	return Md5hex(buf)
}

// Return md5 string for "buf"
func Md5hex(buf []byte) string {
	rawHash := md5.Sum(buf)
	hash := hex.EncodeToString(rawHash[:])
	return hash
}

// Verify that the file size equals "want". This checks:
// 1) Size reported by Stat()
// 2) Number of bytes returned when reading the whole file
func VerifySize(t *testing.T, path string, want int) {
	buf, err := ioutil.ReadFile(path)
	if err != nil {
		t.Errorf("ReadFile failed: %v", err)
	} else if len(buf) != want {
		t.Errorf("wrong read size: got=%d want=%d", len(buf), want)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Errorf("Stat failed: %v", err)
	} else if fi.Size() != int64(want) {
		t.Errorf("wrong stat file size, got=%d want=%d", fi.Size(), want)
	}
}

// Create and delete a directory
func TestMkdirRmdir(t *testing.T, plainDir string) {
	dir := plainDir + "dir1"
	err := os.Mkdir(dir, 0777)
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Rmdir(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Removing a non-empty dir should fail with ENOTEMPTY
	if os.Mkdir(dir, 0777) != nil {
		t.Fatal(err)
	}
	f, err := os.Create(dir + "/file")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	err = syscall.Rmdir(dir)
	errno := err.(syscall.Errno)
	if errno != syscall.ENOTEMPTY {
		t.Errorf("Should have gotten ENOTEMPTY, go %v", errno)
	}
	if syscall.Unlink(dir+"/file") != nil {
		t.Fatal(err)
	}
	if syscall.Rmdir(dir) != nil {
		t.Fatal(err)
	}

	// We should also be able to remove a directory we do not have permissions to
	// read or write
	err = os.Mkdir(dir, 0000)
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Rmdir(dir)
	if err != nil {
		// Make sure the directory can cleaned up by the next test run
		os.Chmod(dir, 0700)
		t.Fatal(err)
	}
}

// Create and rename a file
func TestRename(t *testing.T, plainDir string) {
	file1 := plainDir + "rename1"
	file2 := plainDir + "rename2"
	err := ioutil.WriteFile(file1, []byte("content"), 0777)
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Rename(file1, file2)
	if err != nil {
		t.Fatal(err)
	}
	syscall.Unlink(file2)
}

// verifyExistence - check in 3 ways that "path" exists:
// stat, open, readdir
func VerifyExistence(path string) bool {

	// Check that file can be stated
	_, err := os.Stat(path)
	if err != nil {
		//t.Log(err)
		return false
	}

	// Check that file can be opened
	fd, err := os.Open(path)
	if err != nil {
		//t.Log(err)
		return false
	}
	fd.Close()

	// Check that file shows up in directory listing
	dir := filepath.Dir(path)
	name := filepath.Base(path)
	fi, err := ioutil.ReadDir(dir)
	if err != nil {
		//t.Log(err)
		return false
	}
	for _, i := range fi {
		if i.Name() == name {
			return true
		}
	}
	return false
}
