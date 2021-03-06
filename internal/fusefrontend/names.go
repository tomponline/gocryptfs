package fusefrontend

// This file forwards file encryption operations to cryptfs

import (
	"path/filepath"

	"github.com/rfjakob/gocryptfs/internal/configfile"
	"github.com/rfjakob/gocryptfs/internal/toggledlog"
)

// isFiltered - check if plaintext "path" should be forbidden
//
// Prevents name clashes with internal files when file names are not encrypted
func (fs *FS) isFiltered(path string) bool {
	if !fs.args.PlaintextNames {
		return false
	}
	// gocryptfs.conf in the root directory is forbidden
	if path == configfile.ConfDefaultName {
		toggledlog.Info.Printf("The name /%s is reserved when -plaintextnames is used\n",
			configfile.ConfDefaultName)
		return true
	}
	// Note: gocryptfs.diriv is NOT forbidden because diriv and plaintextnames
	// are exclusive
	return false
}

// GetBackingPath - get the absolute encrypted path of the backing file
// from the relative plaintext path "relPath"
func (fs *FS) getBackingPath(relPath string) (string, error) {
	cPath, err := fs.encryptPath(relPath)
	if err != nil {
		return "", err
	}
	cAbsPath := filepath.Join(fs.args.Cipherdir, cPath)
	toggledlog.Debug.Printf("getBackingPath: %s + %s -> %s", fs.args.Cipherdir, relPath, cAbsPath)
	return cAbsPath, nil
}

// encryptPath - encrypt relative plaintext path
func (fs *FS) encryptPath(plainPath string) (string, error) {
	if fs.args.PlaintextNames {
		return plainPath, nil
	}
	if !fs.args.DirIV {
		return fs.nameTransform.EncryptPathNoIV(plainPath), nil
	}
	fs.dirIVLock.RLock()
	cPath, err := fs.nameTransform.EncryptPathDirIV(plainPath, fs.args.Cipherdir)
	toggledlog.Debug.Printf("encryptPath '%s' -> '%s' (err: %v)", plainPath, cPath, err)
	fs.dirIVLock.RUnlock()
	return cPath, err
}

// decryptPath - decrypt relative ciphertext path
func (fs *FS) decryptPath(cipherPath string) (string, error) {
	if fs.args.PlaintextNames {
		return cipherPath, nil
	}
	if !fs.args.DirIV {
		return fs.nameTransform.DecryptPathNoIV(cipherPath)
	}
	fs.dirIVLock.RLock()
	defer fs.dirIVLock.RUnlock()
	return fs.nameTransform.DecryptPathDirIV(cipherPath, fs.args.Cipherdir)
}
