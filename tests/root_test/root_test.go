//go:build linux

// Package root_test contains tests that need root
// permissions to run
package root_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"

	"github.com/rfjakob/gocryptfs/v2/internal/syscallcompat"

	"github.com/rfjakob/gocryptfs/v2/tests/test_helpers"
)

func asUser(uid int, gid int, supplementaryGroups []int, f func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	err := syscallcompat.Setgroups(supplementaryGroups)
	if err != nil {
		return err
	}
	defer syscallcompat.SetgroupsPanic(nil)

	err = syscallcompat.Setregid(-1, gid)
	if err != nil {
		return err
	}
	defer syscallcompat.SetregidPanic(-1, 0)

	err = syscallcompat.Setreuid(-1, uid)
	if err != nil {
		return err
	}
	defer syscallcompat.SetreuidPanic(-1, 0)

	ret := f()

	// Also reset the saved user id (suid) and saved group id (sgid) to prevent
	// bizarre failures in later tests.
	//
	// Yes, the kernel checks that *all of them* match:
	// https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/tree/fs/fuse/dir.c?h=v5.12-rc2#n1193
	//
	// How to check:
	// ps -o tid,pid,euid,ruid,suid,egid,rgid,sgid,cmd -eL
	_, _, errno := syscall.RawSyscall(syscall.SYS_SETRESUID, uintptr(0), uintptr(0), uintptr(0))
	if errno != 0 {
		panic(errno)
	}
	_, _, errno = syscall.RawSyscall(syscall.SYS_SETRESGID, uintptr(0), uintptr(0), uintptr(0))
	if errno != 0 {
		panic(errno)
	}

	return ret
}

func TestSupplementaryGroups(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("must run as root")
	}

	// We need an unrestricted umask
	syscall.Umask(0000)

	dir1 := test_helpers.DefaultPlainDir + "/dir1"
	err := os.Mkdir(dir1, 0770)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chown(dir1, 0, 1234)
	if err != nil {
		t.Fatal(err)
	}

	err = asUser(1235, 1235, []int{1234}, func() error { return os.Mkdir(dir1+"/dir2", 0700) })
	if err != nil {
		t.Error(err)
	}

	err = asUser(1235, 1235, []int{1234}, func() error {
		f, err := os.Create(dir1 + "/file1")
		if err == nil {
			f.Close()
		}
		return err
	})
	if err != nil {
		t.Error(err)
	}
}

func writeTillFull(t *testing.T, path string) (int, syscall.Errno) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_WRONLY|syscall.O_TRUNC, 0600)
	if err != nil {
		return 0, err.(syscall.Errno)
	}
	defer syscall.Close(fd)
	// Write in 100.000 byte-blocks, which is not aligend to the
	// underlying block size
	buf := make([]byte, 100000)
	var sz int
	for {
		n, err := syscall.Write(fd, buf)
		if err != nil {
			return sz, err.(syscall.Errno)
		}
		sz += n
	}
}

// TestDiskFull needs root permissions because it creates a loop disk
func TestDiskFull(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("must run as root")
	}

	// Create 10 MB file full of zeros
	ext4img := filepath.Join(test_helpers.TmpDir, t.Name()+".ext4")
	f, err := os.Create(ext4img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = f.Truncate(10 * 1024 * 1024)
	if err != nil {
		t.Fatal(err)
	}

	// Format as ext4
	cmd := exec.Command("mkfs.ext4", ext4img)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Log(string(out))
		t.Fatal(err)
	}

	// Mount ext4
	ext4mnt := ext4img + ".mnt"
	err = os.Mkdir(ext4mnt, 0600)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("mount", ext4img, ext4mnt)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Log(string(out))
		t.Fatal(err)
	}
	defer syscall.Unlink(ext4img)
	defer func() {
		const MNT_DETACH = 2
		err := syscall.Unmount(ext4mnt, MNT_DETACH)
		if err != nil {
			t.Log(err)
		}
	}()

	// gocryptfs -init
	cipherdir := ext4mnt + "/a"
	if err = os.Mkdir(cipherdir, 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(test_helpers.GocryptfsBinary, "-q", "-init", "-extpass", "echo test", "-scryptn=10", cipherdir)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Log(string(out))
		t.Fatal(err)
	}

	// Mount gocryptfs
	mnt := ext4mnt + "/b"
	test_helpers.MountOrFatal(t, cipherdir, mnt, "-extpass", "echo test")
	defer test_helpers.UnmountPanic(mnt)

	// Write till we get ENOSPC
	var err1, err2 error
	var sz1, sz2 int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		sz1, err1 = writeTillFull(t, mnt+"/foo1")
		wg.Done()
	}()
	go func() {
		sz2, err2 = writeTillFull(t, mnt+"/foo2")
		wg.Done()
	}()
	wg.Wait()
	if err1 != syscall.ENOSPC || err2 != syscall.ENOSPC {
		t.Fatalf("err1=%v, err2=%v", err1, err2)
	}
	t.Logf("sz1=%d, sz2=%d", sz1, sz2)

	foo1, err := os.ReadFile(mnt + "/foo1")
	if err != nil {
		t.Fatal(err)
	}
	if len(foo1) != sz1 {
		t.Fail()
	}

	foo2, err := os.ReadFile(mnt + "/foo2")
	if err != nil {
		t.Fatal(err)
	}
	if len(foo2) != sz2 {
		t.Fail()
	}
}

func TestAcl(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("must run as root")
	}
	cDir := test_helpers.InitFS(t)
	os.Chmod(cDir, 0755)
	pDir := cDir + ".mnt"
	test_helpers.MountOrFatal(t, cDir, pDir, "-allow_other", "-acl", "-extpass=echo test")
	defer test_helpers.UnmountPanic(pDir)

	f1 := pDir + "/f1"
	if err := os.WriteFile(f1, []byte("hello world\n"), 000); err != nil {
		t.Fatal(err)
	}

	openUser1234 := func(rwMode int) error {
		return asUser(1234, 1234, nil, func() error {
			fd, err := syscall.Open(f1, rwMode, 0)
			if err != nil {
				return err
			}
			defer syscall.Close(fd)
			buf := make([]byte, 100)
			if rwMode == syscall.O_RDONLY || rwMode == syscall.O_RDWR {
				_, err = syscall.Read(fd, buf)
				if err != nil {
					return err
				}
			}
			if rwMode == syscall.O_WRONLY || rwMode == syscall.O_RDWR {
				_, err = syscall.Write(fd, buf)
				if err != nil {
					return err
				}
			}
			return err
		})
	}

	dumpAcl := func() {
		out, err := exec.Command("getfacl", f1).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(out))
	}

	if err := openUser1234(syscall.O_RDONLY); err == nil {
		t.Error("this should have failed")
		dumpAcl()
	}

	// Allow read
	out, err := exec.Command("setfacl", "-m", "u:1234:r", f1).CombinedOutput()
	if err != nil {
		t.Fatal(string(out))
	}
	if err := openUser1234(syscall.O_RDONLY); err != nil {
		t.Errorf("O_RDONLY should have worked, but got error: %v", err)
		dumpAcl()
	}
	if err := openUser1234(syscall.O_WRONLY); err == nil {
		t.Error("O_WRONLY should have failed")
		dumpAcl()
	}

	// Allow write
	out, err = exec.Command("setfacl", "-m", "u:1234:w", f1).CombinedOutput()
	if err != nil {
		t.Fatal(string(out))
	}
	if err := openUser1234(syscall.O_WRONLY); err != nil {
		t.Errorf("O_WRONLY should have worked, but got error: %v", err)
		dumpAcl()
	}
	if err := openUser1234(syscall.O_RDONLY); err == nil {
		t.Error("O_RDONLY should have failed")
		dumpAcl()
	}
}

// TestBtrfsQuirks needs root permissions because it creates a loop disk
func TestBtrfsQuirks(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("must run as root")
	}

	img := filepath.Join(test_helpers.TmpDir, t.Name()+".img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// minimum size for each btrfs device is 114294784
	err = f.Truncate(200 * 1024 * 1024)
	if err != nil {
		t.Fatal(err)
	}

	// Format as Btrfs
	_, err = exec.LookPath("mkfs.btrfs")
	if err != nil {
		t.Skip("mkfs.btrfs not found, skipping test")
	}
	cmd := exec.Command("mkfs.btrfs", img)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("%q", cmd.Args)
		t.Log(string(out))
		t.Fatal(err)
	}

	// Mount
	mnt := img + ".mnt"
	err = os.Mkdir(mnt, 0600)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("mount", img, mnt)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Log(string(out))
		t.Fatal(err)
	}
	defer syscall.Unlink(img)
	defer syscall.Unmount(mnt, 0)

	quirk := syscallcompat.DetectQuirks(mnt)
	if quirk != syscallcompat.QuirkBrokenFalloc {
		t.Errorf("wrong quirk: %v", quirk)
	}
}

func TestOverlay(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("must run as root")
	}
	cDir := test_helpers.InitFS(t)
	if syscallcompat.DetectQuirks(cDir)&syscallcompat.QuirkNoUserXattr != 0 {
		t.Logf("No user xattrs! overlay mount will likely fail.")
	}
	os.Chmod(cDir, 0755)
	pDir := cDir + ".mnt"
	test_helpers.MountOrFatal(t, cDir, pDir, "-allow_other", "-extpass=echo test")
	defer test_helpers.UnmountPanic(pDir)

	for _, d := range []string{"lower", "upper", "work", "merged"} {
		err := os.Mkdir(pDir+"/"+d, 0700)
		if err != nil {
			t.Fatal(err)
		}
	}
	ovlMnt := pDir + "/merged"
	cmd := exec.Command("mount", "-t", "overlay", "overlay",
		"-o", "lowerdir="+pDir+"/lower,upperdir="+pDir+"/upper,workdir="+pDir+"/work",
		ovlMnt)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Log(string(out))
		t.Fatal(err)
	}
	defer syscall.Unmount(ovlMnt, 0)
}

// Check that mkdir and file create works with force_owner and runnung as root
// https://github.com/rfjakob/gocryptfs/issues/783
func TestRootForceOwner(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("must run as root")
	}
	cDir := test_helpers.InitFS(t)
	pDir := cDir + ".mnt"
	test_helpers.MountOrFatal(t, cDir, pDir, "-allow_other", "-extpass=echo test", "-force_owner=1234:1234")
	defer test_helpers.UnmountPanic(pDir)

	err := asUser(1234, 1234, nil, func() error {
		return os.Mkdir(pDir+"/dir1", 0700)
	})
	if err != nil {
		t.Error(err)
	}
	err = asUser(1234, 1234, nil, func() error {
		f, err := os.Create(pDir + "/file1")
		if err == nil {
			f.Close()
		}
		return err
	})
	if err != nil {
		t.Error(err)
	}
	err = asUser(1234, 1234, nil, func() error {
		sock := pDir + "/sock"
		return syscall.Mknod(sock, syscall.S_IFSOCK|0600, 0)
	})
	if err != nil {
		t.Errorf("mknod: %v", err)
	}
}
